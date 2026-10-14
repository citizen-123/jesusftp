package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPasswordPromptProcess(t *testing.T) {
	if os.Getenv("JESUSFTP_PROMPT_TEST_CHILD") != "1" {
		return
	}
	os.Exit(Run([]string{"ftp://saint@127.0.0.1:1", "--plain"}, os.Stdin, os.Stdout, os.Stderr))
}

func TestPasswordTerminalRestoredOnInterruptAndEOF(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		signal os.Signal
		input  string
		code   int
	}{
		{name: "SIGINT", signal: os.Interrupt, code: 130},
		{name: "SIGTERM", signal: syscall.SIGTERM, code: 130},
		{name: "keyboard interrupt", input: "\x03", code: 130},
		{name: "EOF", input: "\x04", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPasswordPromptProcess$")
			cmd.Env = append(os.Environ(), "JESUSFTP_PROMPT_TEST_CHILD=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			ready := make(chan error, 1)
			go func() {
				var prompt [128]byte
				_, err := master.Read(prompt[:])
				ready <- err
			}()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("password prompt did not become ready")
			}
			during, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || during.Lflag&unix.ECHO != 0 {
				t.Fatalf("password input is not hidden: %v", err)
			}
			if tc.signal != nil {
				err = cmd.Process.Signal(tc.signal)
			} else {
				_, err = master.WriteString(tc.input)
			}
			if err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != tc.code || ctx.Err() != nil {
				t.Fatalf("prompt failed to exit with %d before timeout: %v", tc.code, err)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || *after != *before {
				t.Fatalf("prompt did not restore terminal state: %v", err)
			}
		})
	}
}
