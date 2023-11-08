package main

import (
	"bufio"
	"context"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/awoodbeck/gnp/ch09/handlers"
)

var (
	addr    = flag.String("listenHTTP", "127.0.0.1:8080", "listen address for HTTP")
	addrTCP = flag.String("listenTCP", "127.0.0.1:9000", "listen address for TCP")
	cert    = flag.String("cert", "", "certificate")
	pkey    = flag.String("key", "", "private key")
)

func main() {
	flag.Parse()
	err := run(*addr, *addrTCP, *cert, *pkey)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Simple chat server shuts down!")
}

type RegisterForm struct {
	Addr string
	ID   string
}

type client chan<- message // outgoing message chan

type record struct {
	TimeStamp time.Time
	ID        string
	Content   string
}

type message struct {
	ID      string
	Content string
}

type cmd uint8

// command list
const (
	PrintHistory cmd = iota
	ClearHistory
	PrintAllMembers
)

// used global variables
var (
	entering = make(chan client)
	leaving  = make(chan client)
	messages = make(chan message)

	commands  = make(chan cmd, 2)     // some buffer for the command
	blacklist = make(map[string]bool) // record the addr in the blacklist
	add2id    = make(map[string]string)
	id2add    = make(map[string]string)

	history = *new([]record)

	cmdList = map[cmd]string{
		PrintHistory:    "PrintHistory",
		ClearHistory:    "ClearHistory",
		PrintAllMembers: "PrintAllMembers",
	}

	connManager = struct {
		mutex     sync.Mutex
		addr2conn map[string]net.Conn
	}{
		mutex:     sync.Mutex{},
		addr2conn: make(map[string]net.Conn),
	}
)

func run(addr, addrTCP, cert, pkey string) error {
	mux := http.NewServeMux()
	mux.Handle("/register",
		handlers.Methods{
			http.MethodPost: http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					defer func(r io.ReadCloser) {
						_, _ = io.Copy(io.Discard, r) // explicitly drain the request body
						_ = r.Close()
					}(r.Body)

					var u RegisterForm
					err := json.NewDecoder(r.Body).Decode(&u)
					if err != nil {
						http.Error(w, "Decode Failed", http.StatusBadRequest)
						return
					}

					// check whether it's in the blacklist
					if _, ok := blacklist[u.Addr]; ok {
						w.WriteHeader(http.StatusForbidden)
						return
					}

					// check whether it's already connected
					if _, ok := add2id[u.Addr]; ok {
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					w.WriteHeader(http.StatusAccepted)
					w.Write([]byte(addrTCP))
					// register the new addr and id
					// TODO: try to make sure the following TCP connection is comming
					add2id[u.Addr] = u.ID
					id2add[u.ID] = u.Addr

				},
			),

			http.MethodGet: http.HandlerFunc( // tell what information the client should provide
				func(w http.ResponseWriter, r *http.Request) {
					defer func(r io.ReadCloser) {
						_, _ = io.Copy(io.Discard, r) // explicitly drain the request body
						_ = r.Close()
					}(r.Body)

					w.WriteHeader(http.StatusAccepted)
					w.Write([]byte("Should provide a register form with fields: addr and id!"))
				},
			),
		},
	)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		IdleTimeout:       time.Minute,
		ReadHeaderTimeout: 30 * time.Second,
	}

	var err error

	listener, err := net.Listen("tcp", addrTCP)
	if err != nil {
		return err
	}
	go broadCaster()
	done := make(chan struct{})

	var conns sync.WaitGroup // used for ensuring the closure of all the connections

	// handle the os.Interrupt
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)

		for {
			if <-c == os.Interrupt {
				// close the httpserver
				if err := srv.Shutdown(context.Background()); err != nil {
					log.Printf("shutdown: %v", err)
				}

				// close the TCP server
				if err := listener.Close(); err != nil {
					log.Printf("Close TCP server: %v", err)
				}

				// close all the client connections
				connManager.mutex.Lock()
				for _, conn := range connManager.addr2conn {
					conn.Close()
				}
				connManager.mutex.Unlock()

				close(done)
				return
			}
		}
	}()

	// serve the HTTP server
	log.Printf("Serving HTTP over %s\n", srv.Addr)

	go func() {
		if cert != "" && pkey != "" {
			log.Println("TLS enabled")
			err = srv.ListenAndServeTLS(cert, pkey)
		} else {
			err = srv.ListenAndServe()
		}

		if err == http.ErrServerClosed {
			err = nil
		}
	}()

	log.Printf("Serving TCP over %s\n", listener.Addr())
	// serve for TCP clients
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Print(err.Error())
				continue
			}

			// check the addr of the connection
			if _, ok := add2id[conn.RemoteAddr().String()]; !ok {
				// unknown addr
				_ = conn.Close()
				continue
			}

			connManager.mutex.Lock()
			connManager.addr2conn[conn.RemoteAddr().String()] = conn
			connManager.mutex.Unlock()

			conns.Add(1)
			go func(conn net.Conn) {
				defer func() {
					conns.Done()
				}()
				handlerConn(conn)
			}(conn)
		}
	}()

	// read input from stdin
	go func() {
		scanner := bufio.NewScanner(os.Stdin)

		for scanner.Scan() {
			input := scanner.Text()
			num, err := strconv.Atoi(input)
			if err != nil {
				fmt.Println("Please input cmd code!")
				continue
			}
			executeCMD(cmd(num))
		}
	}()

	<-done
	conns.Wait()
	return err
}

func broadCaster() {
	clients := make(map[client]bool)
	printAllCommands()
	for {
		select {
		case msg := <-messages:
			// broadcast all the messages to all other members
			for cli := range clients {
				// TODO: add sensitive message check here
				cli <- msg
			}
			fmt.Printf("%s %s: %s\n", time.Now().String(), msg.ID, msg.Content)
			// record the message
			history = append(history, record{TimeStamp: time.Now(), ID: msg.ID, Content: msg.Content})
		case cli := <-entering:
			// new client
			clients[cli] = true
		case cli := <-leaving:
			// client out
			delete(clients, cli)
			close(cli)
		case cm := <-commands:
			executeCMD(cm)
		}
	}
}

func executeCMD(cm cmd) {
	switch cm {
	case PrintHistory:
		for _, r := range history {
			_, err := fmt.Printf("%s %s: %s\n", r.TimeStamp.String(), r.ID, r.Content)
			if err != nil {
				fmt.Println(err.Error())
				return
			}
		}
	case ClearHistory:
		history = []record{}
		log.Println("Clear the history!")

	case PrintAllMembers:
		for addr, id := range add2id {
			fmt.Printf("Addr: %s, ID: %s\n", addr, id)
		}
		fmt.Printf("%d members in total!\n", len(add2id))

	default:
		fmt.Printf("Unsuppoted command: %d\n", cm)
		printAllCommands()
	}
}

func printAllCommands() {
	fmt.Println("Command list: ")
	for code, name := range cmdList {
		fmt.Printf("Code %d: %s\n", code, name)
	}
}

func handlerConn(conn net.Conn) {
	defer func() {
		connManager.mutex.Lock()
		delete(connManager.addr2conn, conn.RemoteAddr().String())
		connManager.mutex.Unlock()
		conn.Close()

	}()
	ch := make(chan message) // outgoing message sent to the client
	go clientWriter(conn, ch)

	id := add2id[conn.RemoteAddr().String()]
	messages <- message{ID: "host", Content: id + " has arrived!"}
	entering <- ch

	defer func() {
		leaving <- ch
		messages <- message{ID: "host", Content: id + " has leaved!"}
		delete(id2add, id)
		delete(add2id, conn.RemoteAddr().String())
	}()

	dec := gob.NewDecoder(conn)
	var receivedMSG message
	for {
		err := dec.Decode(&receivedMSG)
		messages <- receivedMSG
		if err != nil {
			fmt.Println("Error decoding:", err.Error())
			return
		}
	}

}

func clientWriter(conn net.Conn, ch <-chan message) {
	enc := gob.NewEncoder(conn)
	for msg := range ch {
		err := enc.Encode(msg)
		if err != nil {
			fmt.Println("Error encoding:", err.Error())
		}
	}
}
