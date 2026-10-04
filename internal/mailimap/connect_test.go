package mailimap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func testIMAPServer(t *testing.T, handler func(net.Conn) error) (domain.AccountConfig, *tls.Config, <-chan error) {
	t.Helper()
	certificateServer := httptest.NewTLSServer(nil)
	certificates := certificateServer.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		if accepted != nil {
			_ = accepted.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		mu.Lock()
		accepted = conn
		mu.Unlock()
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := conn.(*tls.Conn).Handshake(); err != nil {
			done <- err
			return
		}
		done <- handler(conn)
	}()
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	account := domain.AccountConfig{Host: host, Port: port, Encryption: domain.EncryptionTLS, Username: "test-user", Password: "test-password"}
	return account, &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}, done
}

func checkIMAPServer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mock server did not finish after connection closed")
	}
}

func TestConnectWaitsForGreetingAndDetachesSetupTimeout(t *testing.T) {
	account, config, done := testIMAPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		firstCommand := make(chan string, 1)
		readError := make(chan error, 1)
		go func() {
			line, err := reader.ReadString('\n')
			if err != nil {
				readError <- err
				return
			}
			firstCommand <- line
		}()
		select {
		case <-firstCommand:
			return errors.New("client sent a command before the server greeting")
		case err := <-readError:
			return err
		case <-time.After(40 * time.Millisecond):
		}
		if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			return err
		}
		var line string
		select {
		case line = <-firstCommand:
		case err := <-readError:
			return err
		}
		for {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return errors.New("invalid mock command")
			}
			var response string
			switch fields[1] {
			case "LOGIN":
				response = fields[0] + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
			case "CAPABILITY":
				response = "* CAPABILITY IMAP4rev1\r\n" + fields[0] + " OK capabilities\r\n"
			case "NOOP":
				response = fields[0] + " OK noop\r\n"
			default:
				return errors.New("unexpected mock command")
			}
			if _, err := fmt.Fprint(conn, response); err != nil {
				return err
			}
			var err error
			line, err = reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
	client, err := connectWithTLSConfig(context.Background(), account, 400*time.Millisecond, 2*time.Second, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// An established transfer connection must outlive the setup-only budget.
	time.Sleep(450 * time.Millisecond)
	if err := client.client.Noop().Wait(); err != nil {
		t.Fatalf("setup timeout closed established connection: %v", err)
	}
	_ = client.Close()
	checkIMAPServer(t, done)
}

func TestConnectBoundsGreetingAndLoginWaits(t *testing.T) {
	for _, stage := range []string{"greeting", "login"} {
		t.Run(stage, func(t *testing.T) {
			account, config, done := testIMAPServer(t, func(conn net.Conn) error {
				reader := bufio.NewReader(conn)
				if stage == "login" {
					if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
						return err
					}
					line, err := reader.ReadString('\n')
					if err != nil {
						return err
					}
					if !strings.Contains(line, " LOGIN ") {
						return errors.New("expected login after greeting")
					}
				}
				line, err := reader.ReadString('\n')
				if line != "" {
					return errors.New("unexpected command while waiting for server response")
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			})
			started := time.Now()
			client, err := connectWithTLSConfig(context.Background(), account, 120*time.Millisecond, 90*time.Second, config)
			if client != nil {
				_ = client.Close()
				t.Fatal("unexpected successful connection")
			}
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("timeout reported as authentication rejection: %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("setup ignored connection timeout")
			}
			checkIMAPServer(t, done)
		})
	}
}

func TestConnectReportsServerAuthenticationRejection(t *testing.T) {
	account, config, done := testIMAPServer(t, func(conn net.Conn) error {
		if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			return err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return err
		}
		tag := strings.Fields(line)[0]
		_, err = fmt.Fprintf(conn, "%s NO [AUTHENTICATIONFAILED] invalid credentials\r\n", tag)
		return err
	})
	client, err := connectWithTLSConfig(context.Background(), account, time.Second, 90*time.Second, config)
	if client != nil {
		_ = client.Close()
		t.Fatal("unexpected successful login")
	}
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("server rejection was misclassified: %v", err)
	}
	checkIMAPServer(t, done)
}
