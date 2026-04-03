package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// LSPTransport is the low-level I/O contract to an LSP subprocess.
// The production implementations wrap gopls and typescript-language-server
// over stdio. Tests inject a stub to avoid requiring external binaries.
//
// All methods must be safe to call from any goroutine, but callers serialise
// access via a mutex so implementations do not need internal locking.
type LSPTransport interface {
	// Send marshals v as JSON and writes it with the Content-Length header frame.
	Send(v interface{}) error
	// Recv reads one Content-Length-framed LSP message and returns it raw.
	Recv() (json.RawMessage, error)
	// Close shuts down the underlying subprocess. Idempotent.
	Close() error
}

// goplsTransport is the production LSPTransport for gopls: a running process
// communicating over stdin/stdout with Content-Length header framing.
type goplsTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
}

// newGoplsTransport starts the gopls binary at goplsPath in the given root
// directory and returns a ready-to-use transport. The caller is responsible
// for sending the LSP Initialize handshake.
func newGoplsTransport(_ context.Context, goplsPath, root string) (LSPTransport, error) {
	cmd := exec.Command(goplsPath)
	cmd.Dir = root

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gopls stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("gopls stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("gopls start: %w", err)
	}

	return &goplsTransport{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(stdout, 64*1024),
	}, nil
}

// newTsserverTransport starts typescript-language-server at tsserverPath in
// the given root directory. The binary must accept the --stdio flag (standard
// for typescript-language-server). The Content-Length framing is identical to
// gopls, so the same goplsTransport struct is reused — only the command differs.
func newTsserverTransport(_ context.Context, tsserverPath, root string) (LSPTransport, error) {
	cmd := exec.Command(tsserverPath, "--stdio")
	cmd.Dir = root

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("tsserver stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("tsserver stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("tsserver start: %w", err)
	}

	return &goplsTransport{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(stdout, 64*1024),
	}, nil
}

// Send marshals v and writes it with LSP Content-Length framing.
// Format: "Content-Length: N\r\n\r\n{json bytes}".
func (t *goplsTransport) Send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal LSP message: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := io.WriteString(t.stdin, header); err != nil {
		return fmt.Errorf("write LSP header: %w", err)
	}
	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("write LSP body: %w", err)
	}
	return nil
}

// Recv reads one LSP message. It parses Content-Length headers, skips unknown
// headers, then reads exactly that many bytes as the JSON body.
func (t *goplsTransport) Recv() (json.RawMessage, error) {
	var contentLength int
	for {
		line, err := t.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read LSP header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line = end of headers
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid Content-Length %q", line)
			}
			contentLength = n
		}
		// Other headers (Content-Type, etc.) are silently ignored.
	}
	if contentLength == 0 {
		return nil, fmt.Errorf("missing Content-Length in LSP message")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(t.reader, body); err != nil {
		return nil, fmt.Errorf("read LSP body (%d bytes): %w", contentLength, err)
	}
	return json.RawMessage(body), nil
}

// Close terminates the gopls/tsserver process. Kill is best-effort; we do not
// propagate the error since Close is called during cleanup where the process
// may already be dead.
func (t *goplsTransport) Close() error {
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
	return nil
}
