package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"syscall"
	"time"
)

type server struct {
	useGorilla bool
}

type contextKey struct {
	key string
}

var ConnContextKey = &contextKey{"http-conn"}

func saveConnInContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, ConnContextKey, c)
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

func main() {
	if len(os.Args) < 2 {
		log.Fatalln("Usage: server <socket-path> [--no-gorilla]")
	}

	socketPath := os.Args[1]
	useGorilla := true
	if len(os.Args) >= 3 && os.Args[2] == "--no-gorilla" {
		useGorilla = false
	}

	srv := &server{
		useGorilla: useGorilla,
	}

	hSrv := http.Server{
		Handler:     srv,
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

func dumpGoroutinesAfter(seconds int) {
	<-time.After(time.Duration(seconds) * time.Second)
	pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
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
		go dumpGoroutinesAfter(3)
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
		go dumpGoroutinesAfter(3)

		// It looks like switching to one or the other of these two lines sometimes gets us past the deadlock,
		// although it's inconsistent. This suggests there might be a race condition somewhere.
		conn, rw, err := http.NewResponseController(w).Hijack()
		//conn, rw, err := w.(http.Hijacker).Hijack()

		if err != nil {
			log.Printf("Error hijacking connection: %v", err)
			return
		}
		log.Println("Connection hijacked")
		defer func(conn net.Conn) {
			err := conn.Close()
			if err != nil {
				log.Printf("Error closing connection: %v", err)
			}
		}(conn)

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
