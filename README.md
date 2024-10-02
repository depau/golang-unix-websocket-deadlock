# Golang unix domain socket WebSocket deadlock reproducer

This is a reproducer for a deadlock that happens when using a WebSocket server over a Unix domain socket in Golang.

## How to reproduce

Build both the client and the server:

```sh
go build -o server ./cmd/server
go build -o client ./cmd/client
```

Run the server:

```sh
./server /tmp/wsunix.sock
```

Run the client:

```sh
./client /tmp/wsunix.sock
```

Observe how the server hangs when it tries to hijack the HTTP connection.

Both the client and the server use Gorilla WebSocket, although I additionally implemented a simple WebSocket to
demonstrate that the issue is likely not caused by Gorilla WebSocket.

To run the server without Gorilla WebSocket, use the `--no-gorilla` flag:

```sh
./server /tmp/wsunix.sock --no-gorilla
```

The culprit seems to be this stack trace where `net/http.(*conn).hijackLocked` tries to abort the background read
goroutine but the goroutine's syscall is never interrupted:

```
1 @ 0x46fbce 0x471199 0x471179 0x47f725 0x6404c5 0x63edef 0x646a77 0x63d314 0x680e92 0x6521ce 0x645890 0x477b41
#       0x471178        sync.runtime_notifyListWait+0x138               /usr/lib/go/src/runtime/sema.go:587
#       0x47f724        sync.(*Cond).Wait+0x84                          /usr/lib/go/src/sync/cond.go:71
#       0x6404c4        net/http.(*connReader).abortPendingRead+0xa4    /usr/lib/go/src/net/http/server.go:738
#       0x63edee        net/http.(*conn).hijackLocked+0x2e              /usr/lib/go/src/net/http/server.go:321
#       0x646a76        net/http.(*response).Hijack+0xd6                /usr/lib/go/src/net/http/server.go:2170
#       0x63d313        net/http.(*ResponseController).Hijack+0x193     /usr/lib/go/src/net/http/responsecontroller.go:71
#       0x680e91        main.(*server).ServeHTTP+0x991                  /home/davide.depau/Projects/unix-socket-ws-hang/cmd/server/main.go:167
#       0x6521cd        net/http.serverHandler.ServeHTTP+0x8d           /usr/lib/go/src/net/http/server.go:3210
#       0x64588f        net/http.(*conn).serve+0x5cf                    /usr/lib/go/src/net/http/server.go:2092
```

Either I'm opening the Unix socket with the wrong (default) flags or there's a bug in the Go standard library.
