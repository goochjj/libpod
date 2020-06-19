// +build !remoteclient

package integration

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/containers/libpod/test/utils"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Podman sdnotify types", func() {
	var (
		tempdir    string
		err        error
		podmanTest *PodmanTestIntegration
	)

	BeforeEach(func() {
		tempdir, err = CreateTempDirInTempDir()
		if err != nil {
			os.Exit(1)
		}
		podmanTest = PodmanTestCreate(tempdir)
		podmanTest.Setup()
		podmanTest.SeedImages()
	})

	AfterEach(func() {
		podmanTest.Cleanup()
		f := CurrentGinkgoTestDescription()
		processTestResult(f)

	})

	It("podman sdnotify ignore with NOTIFY_SOCKET", func() {
		SkipIfRemote()

		sock := filepath.Join(tempdir, "notify")
		addr := net.UnixAddr{
			Name: sock,
			Net:  "unixgram",
		}
		socket, err := net.ListenUnixgram("unixgram", &addr)
		Expect(err).To(BeNil())
		defer os.Remove(sock)
		defer socket.Close()

		os.Setenv("NOTIFY_SOCKET", sock)
		defer os.Unsetenv("NOTIFY_SOCKET")

		session := podmanTest.Podman([]string{"run", "--sdnotify", "ignore", ALPINE, "printenv", "NOTIFY_SOCKET"})
		session.WaitWithDefaultTimeout()
		Expect(session.ExitCode()).To(Equal(1))
		Expect(len(session.OutputToStringArray())).To(Equal(0))
	})

	It("podman container systemd-notify works", func() {
		SkipIfRemote()

		systemdImage := "fedora"
		pull := podmanTest.Podman([]string{"pull", systemdImage})
		pull.WaitWithDefaultTimeout()
		Expect(pull.ExitCode()).To(Equal(0))

		sock := filepath.Join(tempdir, "notify")
		state, err := collectNotifyData(sock)
		Expect(err).To(BeNil())
		defer os.Remove(sock)
		defer state.socket.Close()

		state.podmanExited = false

		os.Setenv("NOTIFY_SOCKET", sock)
		defer os.Unsetenv("NOTIFY_SOCKET")

		session := podmanTest.Podman([]string{"run", systemdImage, "sh", "-c", "ls -ld $NOTIFY_SOCKET; systemd-notify --ready; printenv NOTIFY_SOCKET"})
		session.WaitWithDefaultTimeout()
		state.podmanExited = true
		<-state.doneChannel

		Expect(session.ExitCode()).To(Equal(0))
		Expect(state.sawConmon).To(Equal(2))
		Expect(state.sawReady).To(Equal(1))
		Expect(state.err).ToNot(BeNil())
		Expect(state.err.Error()).To(Equal("OK"))
		Expect(len(session.OutputToStringArray())).To(BeNumerically(">", 0))
	})

	It("podman conmon systemd-notify", func() {
		SkipIfRemote()

		systemdImage := "fedora"
		pull := podmanTest.Podman([]string{"pull", systemdImage})
		pull.WaitWithDefaultTimeout()
		Expect(pull.ExitCode()).To(Equal(0))

		sock := filepath.Join(tempdir, "notify")
		state, err := collectNotifyData(sock)
		Expect(err).To(BeNil())
		defer os.Remove(sock)
		defer state.socket.Close()

		state.podmanExited = false

		os.Setenv("NOTIFY_SOCKET", sock)
		defer os.Unsetenv("NOTIFY_SOCKET")

		session := podmanTest.Podman([]string{"run", "--sdnotify", "conmon", ALPINE, "printenv", "NOTIFY_SOCKET"})
		session.WaitWithDefaultTimeout()
		state.podmanExited = true
		// Wait for collector
		<-state.doneChannel

		Expect(session.ExitCode()).To(Equal(1))
		Expect(state.sawConmon).To(Equal(2))
		Expect(state.sawReady).To(Equal(1))
		Expect(state.err).ToNot(BeNil())
		Expect(state.err.Error()).To(Equal("OK"))
		Expect(len(session.OutputToStringArray())).To(Equal(0))
	})
})

type notifyState struct {
	socket       *net.UnixConn
	podmanExited bool
	sawConmon    int
	sawMainpid   int
	sawReady     int
	err          error
	doneChannel  chan bool
}

// Manage the notify socket
// Count the MAINPID and READY messages
// Verify they point at podman
// Report errors
func collectNotifyData(sockpath string) (*notifyState, error) {
	state := notifyState{nil, false, 0, 0, 0, nil, make(chan bool)}

	addr := net.UnixAddr{
		Name: sockpath,
		Net:  "unixgram",
	}
	socket, err := net.ListenUnixgram("unixgram", &addr)
	state.socket = socket
	if err != nil {
		return &state, err
	}
	go func() {
		var buf [1024]byte
		last := false
		for {
			state.socket.SetReadDeadline(time.Now().Add(2 * time.Second))

			n, err := state.socket.Read(buf[:])
			if err != nil {
				if e, ok := err.(net.Error); !ok || !e.Timeout() {
					// handle error, it's not a timeout
					state.err = err
					break
				}
				if last {
					state.err = errors.New("OK")
					break
				}
				last = state.podmanExited
				continue
			}
			if n <= 0 {
				state.err = errors.New("End of File")
				break
			}

			s := string(buf[:n])
			for _, field := range strings.Split(s, "\n") {
				fmt.Println(field)
				if len(field) > 0 {
					if strings.HasPrefix(field, "MAINPID=") {
						state.sawMainpid++
						pid := field[8:]
						l, err := os.Readlink(filepath.Join("/proc/", pid, "/exe"))
						if err == nil && filepath.Base(l) == "conmon" {
							state.sawConmon++
						}
					} else if field == "READY=1" {
						state.sawReady++
					}
				}
			}
		}
		state.doneChannel <- true
	}()

	return &state, nil
}
