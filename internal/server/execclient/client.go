package execclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/containerd/console"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/go-hclog"
	grpc_net_conn "github.com/mitchellh/go-grpc-net-conn"
	sshterm "golang.org/x/crypto/ssh/terminal"

	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
)

type Client struct {
	Logger        hclog.Logger
	UI            terminal.UI
	Context       context.Context
	Client        pb.WaypointClient
	DeploymentId  string
	DeploymentSeq uint64
	Args          []string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
}

func (c *Client) Run() (int, error) {
	// Determine if we should allocate a pty. If we should, we need to send
	// along a TERM value to the remote end that matches our own.
	var ptyReq *pb.ExecStreamRequest_PTY
	var ptyF *os.File
	var status terminal.Status

	if f, ok := c.Stdout.(*os.File); ok && sshterm.IsTerminal(int(f.Fd())) {
		status = c.UI.Status()
		defer status.Close()
		status.Update(fmt.Sprintf("Connecting to deployment v%d...", c.DeploymentSeq))

		ptyF = f
		c, err := console.ConsoleFromFile(ptyF)
		if err != nil {
			return 0, err
		}

		sz, err := c.Size()
		c = nil
		if err != nil {
			return 0, err
		}

		ptyReq = &pb.ExecStreamRequest_PTY{
			Enable: true,
			Term:   os.Getenv("TERM"),
			WindowSize: &pb.ExecStreamRequest_WindowSize{
				Rows:   int32(sz.Height),
				Cols:   int32(sz.Width),
				Height: int32(sz.Height),
				Width:  int32(sz.Width),
			},
		}
	}

	// Start our exec stream
	client, err := c.Client.StartExecStream(c.Context)
	if err != nil {
		return 0, err
	}

	defer client.CloseSend()

	if status != nil {
		status.Update("Initializing session...")
	}

	// Send the start event
	if err := client.Send(&pb.ExecStreamRequest{
		Event: &pb.ExecStreamRequest_Start_{
			Start: &pb.ExecStreamRequest_Start{
				DeploymentId: c.DeploymentId,
				Args:         c.Args,
				Pty:          ptyReq,
			},
		},
	}); err != nil {
		return 0, err
	}

	if status != nil {
		status.Update("Waiting for instance assignment...")
	}

	// Receive our open message. If this fails then we weren't assigned.
	resp, err := client.Recv()
	if err != nil {
		return 1, err
	}
	if _, ok := resp.Event.(*pb.ExecStreamResponse_Open_); !ok {
		return 1, fmt.Errorf("internal protocol error: unexpected opening message")
	}

	if ptyF != nil {
		status.Close()
		c.UI.Output("Connected to deployment v%d", c.DeploymentSeq, terminal.WithSuccessStyle())
	}

	// Close our UI if we can
	if closer, ok := c.UI.(io.Closer); ok {
		closer.Close()
	}

	if ptyF != nil {
		// We need to go into raw mode with stdin
		if f, ok := c.Stdin.(*os.File); ok {
			oldState, err := sshterm.MakeRaw(int(f.Fd()))
			if err != nil {
				return 0, err
			}
			defer sshterm.Restore(int(f.Fd()), oldState)
		}

		fmt.Fprintf(c.Stdout, "\r")
	}

	// Create the context that we'll listen to that lets us cancel our
	// extra goroutines here.
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()

	input := &EscapeWatcher{Cancel: cancel, Input: c.Stdin}

	// Build our connection. We only build the stdin sending side because
	// we can receive other message types from our recv.
	go io.Copy(&grpc_net_conn.Conn{
		Stream:  client,
		Request: &pb.ExecStreamRequest{},
		Encode: grpc_net_conn.SimpleEncoder(func(msg proto.Message) *[]byte {
			req := msg.(*pb.ExecStreamRequest)
			if req.Event == nil {
				req.Event = &pb.ExecStreamRequest_Input_{
					Input: &pb.ExecStreamRequest_Input{},
				}
			}

			return &req.Event.(*pb.ExecStreamRequest_Input_).Input.Data
		}),
	}, input)

	// Add our recv blocker that sends data
	recvCh := make(chan *pb.ExecStreamResponse)
	go func() {
		defer cancel()
		for {
			resp, err := client.Recv()
			if err != nil {
				c.Logger.Error("receive error", "err", err)
				return
			}

			recvCh <- resp
		}
	}()

	// Listen for window change events
	winchCh := make(chan os.Signal, 1)
	registerSigwinch(winchCh)
	defer signal.Stop(winchCh)

	// Loop for data
	for {
		select {
		case resp := <-recvCh:
			switch event := resp.Event.(type) {
			case *pb.ExecStreamResponse_Output_:
				// TODO: stderr
				out := c.Stdout
				io.Copy(out, bytes.NewReader(event.Output.Data))

			case *pb.ExecStreamResponse_Exit_:
				return int(event.Exit.Code), nil

			default:
				c.Logger.Warn("unknown event type",
					"type", fmt.Sprintf("%T", resp.Event))
			}

		case <-winchCh:
			// Window change, send new size
			c, err := console.ConsoleFromFile(ptyF)
			if err != nil {
				continue
			}

			sz, err := c.Size()
			if err != nil {
				continue
			}

			// Send the new window size
			if err := client.Send(&pb.ExecStreamRequest{
				Event: &pb.ExecStreamRequest_Winch{
					Winch: &pb.ExecStreamRequest_WindowSize{
						Rows:   int32(sz.Height),
						Cols:   int32(sz.Width),
						Height: int32(sz.Height),
						Width:  int32(sz.Width),
					},
				},
			}); err != nil {
				// Ignore this error
				continue
			}

		case <-ctx.Done():
			return 1, nil
		}
	}
}
