package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall"
	"time"
)

func main() {
	log.Println("GOOS:", runtime.GOOS)
	log.Println("GOARCH:", runtime.GOARCH)

	noGorilla := true
	socketPath := "/tmp/test.sock"

	go serverMain(socketPath, !noGorilla)

	go func() {
		<-time.After(3 * time.Second)
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		log.Println("Forcefully exiting")
		_ = os.RemoveAll(socketPath)
		os.Exit(0)
	}()

	// Give the server some time to start listening
	time.Sleep(500 * time.Millisecond)

	log.Println("Running client")
	clientMain(socketPath)
	log.Println("Client exited normally")
}

////////////////////////////////////////////////////////////////////////////////
// Server
////////////////////////////////////////////////////////////////////////////////

type server struct {
	useGorilla bool
}

type contextKey struct {
	key string
}

var ConnContextKey = &contextKey{"http-conn"}
var ConnContextCancelKey = &contextKey{"http-conn-cancel"}

func saveConnInContext(ctx context.Context, c net.Conn) context.Context {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	ctx = context.WithValue(ctx, ConnContextKey, c)
	ctx = context.WithValue(ctx, ConnContextCancelKey, cancel)
	return ctx
}

func getHttpConn(r *http.Request) *net.UnixConn {
	return r.Context().Value(ConnContextKey).(*net.UnixConn)
}

var upgrader = websocket.Upgrader{}

var keyGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	h.Write([]byte(challengeKey))
	h.Write(keyGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func serverMain(socketPath string, useGorilla bool) {
	_ = os.Remove(socketPath)

	srv := &server{
		useGorilla: useGorilla,
	}

	hSrv := http.Server{
		Handler:     srv,
		ReadTimeout: 100 * time.Millisecond,
		ConnContext: saveConnInContext,
	}

	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("Error listening on unix socket: %v", err)
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
		<-sigint

		// We received an interrupt signal, shut down.
		if err := hSrv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Printf("HTTP server Shutdown: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Println("Listening on", socketPath)
	if err := hSrv.Serve(unixListener); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Error serving: %v", err)
	}

	<-idleConnsClosed
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn := getHttpConn(r)
	if file, err := conn.File(); err == nil {
		if ucred, err := syscall.GetsockoptUcred(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err != nil {
			log.Printf("Error getting peer credentials: %v", err)
		} else {
			log.Printf("Connection from: %v", ucred)
		}
	} else {
		log.Printf("Error getting peer credentials: %v", err)
	}

	connHeader := r.Header.Get("Connection")
	if connHeader != "Upgrade" {
		// Handle regular HTTP request
		body := fmt.Sprintf("Hello, World! Received %s %s", r.Method, r.URL.Path)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, err := w.Write([]byte(body))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// Handle WebSocket upgrade
	if s.useGorilla {
		// Use Gorilla WebSocket
		log.Println("Upgrading to WebSocket")
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Error upgrading to WebSocket: %v", err)
			return
		}
		log.Println("WebSocket upgraded")
		defer func(ws *websocket.Conn) {
			err := ws.Close()
			if err != nil {
				log.Printf("Error closing WebSocket: %v", err)
			}
		}(ws)

		err = ws.WriteMessage(websocket.TextMessage, []byte("Hello, World!"))
		if err != nil {
			log.Printf("Error writing message: %v", err)
			return
		}

		for {
			mt, message, err := ws.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				break
			}
			log.Printf("recv: %s", message)
			err = ws.WriteMessage(mt, message)
			if err != nil {
				log.Println("write:", err)
				break
			}
		}
	} else {
		// Use manual WebSocket upgrade
		log.Println("Hijacking connection")

		// Steal the file descriptor first
		connFile, err := conn.File()
		if err != nil {
			log.Printf("Error getting file descriptor: %v", err)
			return
		}
		log.Println("Duping file descriptor")
		connFd := int(connFile.Fd())
		newFd, err := syscall.Dup(connFd)
		if err != nil {
			log.Printf("Error duplicating file descriptor: %v", err)
			return
		}
		log.Println("Duplicated file descriptor")

		defused := false
		go func() {
			<-time.After(100 * time.Millisecond)
			if !defused {
				log.Println("Forcefully closing hijacked socket")
				err := syscall.Close(connFd)
				if err != nil {
					log.Printf("Error closing hijacked socket: %v", err)
				}
				log.Println("Hijacked socket closed")
			}
		}()
		go func() {
			_, _, _ = http.NewResponseController(w).Hijack()
			defused = true
			log.Println("Connection hijacked")
		}()
		//conn, rw, err := w.(http.Hijacker).Hijack()

		conn, err := net.FileConn(os.NewFile(uintptr(newFd), ""))
		if err != nil {
			log.Printf("Failed to create new connection: %v", err)
			return
		}
		log.Println("Connection created")
		defer func(conn net.Conn) {
			err := conn.Close()
			if err != nil {
				log.Printf("Error closing connection: %v", err)
			}
		}(conn)

		rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

		// Write status line
		challengeKey := r.Header.Get("Sec-WebSocket-Key")
		_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + computeAcceptKey(challengeKey) + "\r\n\r\n")

		log.Println("Wrote status line and upgraded to WebSocket, sending message")

		// Send a simple text message
		message := []byte("Hello, World!")

		// 1000 0001: FIN | Text opcode
		if err := rw.WriteByte(0x81); err != nil {
			log.Printf("Error writing message: %v", err)
			return
		}
		if err := rw.WriteByte(byte(len(message))); err != nil {
			log.Printf("Error writing message: %v", err)
			return
		}
		if _, err := rw.Write(message); err != nil {
			log.Printf("Error writing message: %v", err)
			return
		}

		if err := rw.Flush(); err != nil {
			log.Printf("Error flushing response: %v", err)
			return
		}

		log.Println("Message sent, closing connection")
		return
	}
}

////////////////////////////////////////////////////////////////////////////////
// Client
////////////////////////////////////////////////////////////////////////////////

func clientMain(socket string) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	u := url.URL{Scheme: "ws", Host: "test", Path: "/ws"}
	log.Printf("connecting to %s", u.String())

	conn, err := net.Dial("unix", socket)
	if err != nil {
		log.Fatalf("Error connecting to unix socket: %v", err)
	}

	c, _, err := websocket.NewClient(conn, &u, nil, 1024, 1024)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer func(c *websocket.Conn) {
		err := c.Close()
		if err != nil {
			log.Fatalf("Error closing websocket: %v", err)
		}
	}(c)

	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				return
			}
			log.Printf("recv: %s", message)
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	start := time.Now()

	for {
		select {
		case <-done:
			return
		case t := <-ticker.C:
			err := c.WriteMessage(websocket.TextMessage, []byte(t.String()))
			if err != nil {
				log.Println("write:", err)
				return
			}
			if t.Sub(start) > 2*time.Second {
				close(interrupt)
			}
		case <-interrupt:
			log.Println("interrupt")

			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("write close:", err)
				return
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}
