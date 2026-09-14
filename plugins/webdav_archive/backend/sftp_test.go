package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startTestSFTPServer runs a real SSH server serving a real SFTP subsystem over
// a temporary directory, so the transport is exercised end to end rather than
// against a mock of itself. It returns the address and the directory it serves.
func startTestSFTPServer(t *testing.T, username, password string) (string, string) {
	t.Helper()
	root := t.TempDir()

	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if conn.User() == username && string(given) == password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSFTPConn(conn, config, root)
		}
	}()
	return listener.Addr().String(), root
}

func serveSFTPConn(conn net.Conn, config *ssh.ServerConfig, root string) {
	defer conn.Close()
	serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range channelRequests {
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 &&
					string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}()
		go func(channel ssh.Channel) {
			defer channel.Close()
			server, err := sftp.NewServer(channel, sftp.WithServerWorkingDirectory(root))
			if err != nil {
				return
			}
			_ = server.Serve()
		}(channel)
	}
}

func openTestSFTPStore(t *testing.T, addr, root string) remoteStore {
	t.Helper()
	address, err := parseTargetAddress("sftp://" + addr + "/" + root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newSFTPStore(context.Background(), address, "archivist", "hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSFTPStoreRoundTripsAFile(t *testing.T) {
	addr, root := startTestSFTPServer(t, "archivist", "hunter2")
	store := openTestSFTPStore(t, addr, root)
	ctx := context.Background()

	// The directories above the file do not exist yet, which is the ordinary
	// case for the first recording of a month.
	if err := store.Put(ctx, "2026/05/voice memo.m4a", []byte("bytes"), "audio/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026", "05", "voice memo.m4a")); err != nil {
		t.Fatalf("the file is not where the path said: %v", err)
	}

	exists, err := store.Exists(ctx, "2026/05/voice memo.m4a")
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	if exists, err := store.Exists(ctx, "2026/05/gone.m4a"); err != nil || exists {
		t.Fatalf("Exists(gone) = %v, %v", exists, err)
	}

	entries, err := store.List(ctx, "2026/05")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "voice memo.m4a" || entries[0].IsDir {
		t.Fatalf("listing = %+v", entries)
	}
	// A path from a listing has to be one the download route can fetch again,
	// and the type has to be named or the recording will not play in place.
	if entries[0].Path != "2026/05/voice memo.m4a" {
		t.Fatalf("entry path = %q, want it relative to the target root", entries[0].Path)
	}
	if entries[0].ContentType != "audio/mp4" {
		t.Fatalf("content type = %q", entries[0].ContentType)
	}
	if entries[0].Size != 5 || entries[0].ModifiedAt == 0 {
		t.Fatalf("entry = %+v, want size and modified time read from the server", entries[0])
	}

	body, contentType, size, err := store.Get(ctx, entries[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "bytes" || contentType != "audio/mp4" || size != 5 {
		t.Fatalf("get = %q, %q, %d", got, contentType, size)
	}

	if err := store.Delete(ctx, entries[0].Path); err != nil {
		t.Fatal(err)
	}
	if exists, _ := store.Exists(ctx, entries[0].Path); exists {
		t.Fatal("the file survived its delete")
	}
}

func TestSFTPStoreListsFoldersFirstAndSorted(t *testing.T) {
	addr, root := startTestSFTPServer(t, "archivist", "hunter2")
	store := openTestSFTPStore(t, addr, root)
	ctx := context.Background()
	for _, name := range []string{"b.m4a", "a.m4a"} {
		if err := store.Put(ctx, name, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(ctx, "zzz/inner.m4a", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	// A browser that reshuffles between refreshes is unusable, so the order is
	// part of what this returns: folders first, then by name.
	if strings.Join(names, ",") != "zzz,a.m4a,b.m4a" {
		t.Fatalf("order = %v", names)
	}
	if !entries[0].IsDir || entries[0].Path != "zzz/" {
		t.Fatalf("folder entry = %+v, want a trailing slash so the browser can descend", entries[0])
	}
}

// Nothing a caller passes may address anything above the configured root.
func TestSFTPStoreCannotEscapeItsRoot(t *testing.T) {
	addr, serverRoot := startTestSFTPServer(t, "archivist", "hunter2")
	if err := os.MkdirAll(filepath.Join(serverRoot, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "secret.txt"), []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	address, err := parseTargetAddress("sftp://" + addr + "/" + serverRoot + "/archive")
	if err != nil {
		t.Fatal(err)
	}
	store, err := newSFTPStore(context.Background(), address, "archivist", "hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, escape := range []string{"../secret.txt", "../../secret.txt", "a/../../secret.txt"} {
		if _, _, _, err := store.Get(ctx, escape); err == nil {
			t.Fatalf("Get(%q) reached a file above the configured root", escape)
		}
	}
	// A write that tries to climb out lands inside instead of failing, which is
	// the same rule the path template follows.
	if err := store.Put(ctx, "../escaped.m4a", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(serverRoot, "escaped.m4a")); err == nil {
		t.Fatal("a write climbed above the configured root")
	}
	if _, err := os.Stat(filepath.Join(serverRoot, "archive", "escaped.m4a")); err != nil {
		t.Fatalf("the write did not land inside the root either: %v", err)
	}
}

func TestSFTPStoreRefusesWrongCredentials(t *testing.T) {
	addr, root := startTestSFTPServer(t, "archivist", "hunter2")
	address, err := parseTargetAddress("sftp://" + addr + "/" + root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSFTPStore(context.Background(), address, "archivist", "wrong", ""); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if _, err := newSFTPStore(context.Background(), address, "", "hunter2", ""); err == nil {
		t.Fatal("a target with no user name was accepted")
	}
	if _, err := newSFTPStore(context.Background(), address, "archivist", "", ""); err == nil {
		t.Fatal("a target with no credential at all was accepted")
	}
}

// The dial guard is one decision for all three transports, not just the HTTP
// one it was written for.
func TestSFTPStoreObeysTheDialGuard(t *testing.T) {
	t.Setenv("ROLLTOP_WEBDAV_ALLOW_PRIVATE_HOSTS", "0")
	addr, root := startTestSFTPServer(t, "archivist", "hunter2")
	address, err := parseTargetAddress("sftp://" + addr + "/" + root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSFTPStore(context.Background(), address, "archivist", "hunter2", ""); err == nil {
		t.Fatal("loopback was dialed with private hosts turned off")
	}
}
