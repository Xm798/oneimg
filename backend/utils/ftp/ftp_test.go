package ftp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCreateReadDeleteUseLoginRelativeKeys(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		reply := func(value string) { _, _ = fmt.Fprintf(conn, "%s\r\n", value) }
		reply("220 test FTP")
		files := map[string][]byte{}
		var passive net.Listener
		defer func() {
			if passive != nil {
				_ = passive.Close()
			}
		}()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			command, arg, _ := strings.Cut(scanner.Text(), " ")
			switch command {
			case "USER":
				reply("331 password required")
			case "PASS":
				reply("230 logged in")
			case "FEAT":
				reply("211 no extensions")
			case "TYPE", "OPTS", "NOOP":
				reply("200 OK")
			case "PWD":
				reply(`257 "/home/test"`)
			case "EPSV", "PASV":
				passive, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				port := passive.Addr().(*net.TCPAddr).Port
				if command == "EPSV" {
					reply(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", port))
				} else {
					reply(fmt.Sprintf("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256))
				}
			case "MKD", "LIST", "STOR", "RETR", "DELE":
				if strings.HasPrefix(arg, "/") {
					t.Error("object operation escaped the login directory", command, arg)
					reply("550 absolute paths are not writable")
					continue
				}
				if command == "MKD" {
					reply(`257 "created"`)
					continue
				}
				if command == "DELE" {
					delete(files, arg)
					reply("250 deleted")
					continue
				}
				if passive == nil {
					t.Error("no passive data connection")
					return
				}
				reply("150 transferring")
				data, err := passive.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				_ = data.SetDeadline(time.Now().Add(5 * time.Second))
				switch command {
				case "LIST":
					for key, body := range files {
						if path.Dir(key) == arg {
							_, _ = fmt.Fprintf(data, "-rw-r--r-- 1 owner group %d Jan 01 00:00 %s\r\n", len(body), path.Base(key))
						}
					}
				case "STOR":
					files[arg], err = io.ReadAll(data)
					if err != nil {
						t.Error(err)
					}
				case "RETR":
					_, _ = data.Write(files[arg])
				}
				_ = data.Close()
				_ = passive.Close()
				passive = nil
				reply("226 complete")
			case "QUIT":
				reply("221 goodbye")
				return
			default:
				t.Errorf("unexpected FTP command: %s", scanner.Text())
				reply("502 unsupported")
			}
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	client := NewFTPUtil(FTPConfig{Host: host, Port: portNumber, User: "test", Password: "test-only", Timeout: 5})
	defer func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("test FTP server did not stop")
		}
	}()
	key := "/uploads/2026/photo.png"
	if err := client.UploadCreate(context.Background(), key, bytes.NewReader([]byte("original"))); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadCreate(context.Background(), key, bytes.NewReader([]byte("replacement"))); err == nil {
		t.Fatal("existing object was overwritten")
	}
	conn, err := client.GetClient()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := conn.Retr(ObjectPath(key))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	// The data transfer must finish before the shared control connection accepts the next command.
	_ = reader.Close()
	if err != nil || string(body) != "original" {
		t.Fatal("read did not use the created object", string(body), err)
	}
	if err := client.DeleteImage(key); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadCreate(context.Background(), key, bytes.NewReader([]byte("new"))); err != nil {
		t.Fatal("delete did not remove the same object", err)
	}
}
