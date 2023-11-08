package main

import (
	"bufio"
	"bytes"
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
	"time"
)

var (
	remoteAddr = flag.String("remote_addr", "127.0.0.1:8080", "remote addr for remote server")
	localAddr  = flag.String("local_addr", "127.0.0.1:10004", "local addr for TCP connection")
	id         = flag.String("id", "John", "id name for the user")
)

func main() {
	flag.Parse()

	// make a post to register to the chatserver
	buf := new(bytes.Buffer)
	form := RegisterForm{
		Addr: *localAddr,
		ID:   *id,
	}
	err := json.NewEncoder(buf).Encode(&form)
	if err != nil {
		log.Fatal(err)
	}
	// filepath.Join(*remoteAddr, "register")
	resp, err := http.Post("http://"+*remoteAddr+"/register", "application/json", buf)
	if err != nil {
		log.Fatal(err)
	}
	// handle the response
	if resp.StatusCode != http.StatusAccepted {
		log.Fatalf("expected status code %d; actual status code %d",
			http.StatusAccepted, resp.StatusCode)
	}
	buf.Reset()
	n, err := io.Copy(buf, resp.Body)
	defer resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}
	if n == 0 {
		log.Fatal("expected resp larger than 0!")
	}
	// fmt.Println("Successful in getting the response!")
	// make a TCP connection
	TCPaddr := buf.String()
	tempAddr, err := net.ResolveTCPAddr("tcp", *localAddr)
	if err != nil {
		log.Fatal("Encounter an error when parseing the local addr as TCP addr!")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialer := net.Dialer{
		Timeout:   10 * time.Second,
		LocalAddr: tempAddr,
	}
	conn, err := dialer.DialContext(ctx, "tcp", TCPaddr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Successful in connecting the chatserver!")
	done := make(chan struct{})
	defer func() {
		conn.Close()
		<-done
	}()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)

		for {
			if <-c == os.Interrupt {
				cancel()
			}
		}
	}()

	go func() {
		dec := gob.NewDecoder(conn)
		var receivedMSG message

		for {
			err := dec.Decode(&receivedMSG)
			if err != nil {
				fmt.Println("Error decoding:", err.Error())
				break
			}
			fmt.Printf("%s %s: %s\n", time.Now().String(), receivedMSG.ID, receivedMSG.Content)
		}
		log.Println("done")
		done <- struct{}{}
	}()
	mustCopy(conn, os.Stdin, *id)

}

type RegisterForm struct {
	Addr string
	ID   string
}

type message struct {
	ID      string
	Content string
}

func mustCopy(dst io.Writer, src io.Reader, id string) {
	enc := gob.NewEncoder(dst)
	scanner := bufio.NewScanner(src)
	var msg message
	msg.ID = id

	for scanner.Scan() {
		msg.Content = scanner.Text()
		err := enc.Encode(msg)
		if err != nil {
			fmt.Println("Error encoding:", err.Error())
			break
		}
	}
}
