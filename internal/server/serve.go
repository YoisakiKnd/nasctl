package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/webstate"
)

// Serve runs the HTTP service until ctx is cancelled.
//
// The timeouts are set explicitly rather than left at zero. A read timeout
// bounds how long a slow or hostile client can hold one of the (few) request
// slots; a write timeout bounds how long a stalled client can hold a streaming
// descriptor. Zero — Go's default — means "forever", which on a machine with a
// handful of file descriptors is not a safe default.
func Serve(ctx context.Context, cfg *config.Config, s *Server, diag io.Writer) error {
	ln, err := net.Listen("tcp", cfg.Server.Bind)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", cfg.Server.Bind, err)
	}

	// Two web services over one index would each hold their own handle and
	// neither would know about the other, so a live record is a refusal.
	if prev, err := webstate.Read(cfg.Paths.RunDir); err == nil &&
		webstate.Alive(webstate.ProcRoot(), prev.PID) {
		ln.Close()
		return fmt.Errorf("a web service is already running on %s (pid %d, started %s); "+
			"stop it first, or run this one with a different paths.run_dir",
			prev.Bind, prev.PID, prev.Started)
	}

	// The service identity check comes after the listener exists so that the
	// log line can state the address that is actually in use.
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	// Record ourselves so `albumctl status` can report this process's memory
	// without asking it anything. Failing to write the record is not fatal: the
	// service is more useful than the bookkeeping about it.
	recErr := webstate.Write(cfg.Paths.RunDir, webstate.Record{
		PID:      os.Getpid(),
		Bind:     ln.Addr().String(),
		Started:  time.Now().Format(time.RFC3339),
		Version:  buildinfo.Version,
		Schema:   buildinfo.Schema,
		ReadOnly: true,
	})
	if recErr != nil {
		fmt.Fprintf(diag, "  warning: cannot record the service in %s: %v\n",
			webstate.Path(cfg.Paths.RunDir), recErr)
	} else {
		defer webstate.Remove(cfg.Paths.RunDir)
	}
	fmt.Fprintf(diag, "albumctl web service listening on %s:%s\n", host, port)
	fmt.Fprintf(diag, "  read-only: it cannot scan, decode, generate a thumbnail or modify the index\n")
	if cfg.Server.Token == "" {
		fmt.Fprintf(diag, "  no token is configured; access depends entirely on reaching this address\n")
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback():
			fmt.Fprintf(diag, "  bound to loopback: reachable only from this machine and through a tunnel\n")
		case ip.To4() != nil && ip.To4()[0] == 100:
			fmt.Fprintf(diag, "  bound to the Tailscale range: reachable only by devices in your tailnet\n")
		default:
			fmt.Fprintf(diag, "  bound to a private address %s; other devices on that network can reach it\n", host)
		}
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          nil,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// A bounded graceful shutdown. A held-open video stream must not be allowed
	// to delay the exit indefinitely, so a deadline is imposed and the service
	// then closes whatever is left.
	shutdownTimeout := cfg.Server.ShutdownTimeout.D()
	if shutdownTimeout <= 0 {
		shutdownTimeout = 5 * time.Second
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	fmt.Fprintf(diag, "shutting down (up to %s)\n", shutdownTimeout)
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown after %s: %w", shutdownTimeout, err)
	}
	return nil
}
