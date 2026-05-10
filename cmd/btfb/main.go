package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type result struct {
	iteration int
	latency   time.Duration
}

func main() {
	var (
		launchCmd   = flag.String("launch", "", "command used to start the unikernel instance")
		targetAddr  = flag.String("target", "127.0.0.1:8080", "address exposed by the proxy")
		iterations  = flag.Int("n", 5, "number of measurements")
		timeout     = flag.Duration("timeout", 10*time.Second, "timeout per measurement")
		readBytes   = flag.Int("read-bytes", 1, "bytes to read before considering the first byte observed")
		startDelay  = flag.Duration("start-delay", 0, "optional delay after launch before probing")
		killWait    = flag.Duration("kill-wait", 1500*time.Millisecond, "grace period before sending SIGKILL")
		launchShell = flag.String("shell", "/bin/sh", "shell used to launch the command")
	)
	flag.Parse()

	if strings.TrimSpace(*launchCmd) == "" {
		fmt.Fprintln(os.Stderr, "launch command is required")
		os.Exit(2)
	}
	if *iterations <= 0 {
		fmt.Fprintln(os.Stderr, "n must be > 0")
		os.Exit(2)
	}
	if *readBytes <= 0 {
		fmt.Fprintln(os.Stderr, "read-bytes must be > 0")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	results := make([]result, 0, *iterations)
	var total time.Duration

	for i := 1; i <= *iterations; i++ {
		latency, err := measureOnce(ctx, *launchShell, *launchCmd, *targetAddr, *timeout, *startDelay, *killWait, *readBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "iteration %d failed: %v\n", i, err)
			os.Exit(1)
		}
		results = append(results, result{iteration: i, latency: latency})
		total += latency
		fmt.Printf("iteration=%d btfb_ms=%.3f\n", i, float64(latency.Microseconds())/1000.0)
	}

	avg := total / time.Duration(len(results))
	fmt.Printf("summary count=%d avg_ms=%.3f\n", len(results), float64(avg.Microseconds())/1000.0)
}

func measureOnce(
	parent context.Context,
	shell string,
	launchCmd string,
	targetAddr string,
	timeout time.Duration,
	startDelay time.Duration,
	killWait time.Duration,
	readBytes int,
) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-lc", launchCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	defer terminateProcess(cmd, killWait)

	if startDelay > 0 {
		select {
		case <-time.After(startDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	if err := waitForFirstByte(ctx, targetAddr, readBytes); err != nil {
		return 0, err
	}

	return time.Since(startedAt), nil
}

func waitForFirstByte(ctx context.Context, targetAddr string, readBytes int) error {
	probeBuf := make([]byte, readBytes)
	for {
		conn, err := (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext(ctx, "tcp", targetAddr)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
		reader := bufio.NewReader(conn)
		_, err = ioReadFullCompat(reader, probeBuf)
		_ = conn.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func terminateProcess(cmd *exec.Cmd, killWait time.Duration) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(killWait):
		_ = cmd.Process.Kill()
		<-done
	}
}

func ioReadFullCompat(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if errors.Is(err, net.ErrClosed) && total > 0 {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}
