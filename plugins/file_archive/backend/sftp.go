// File overview: The SFTP transport.
//
// This is the one that reaches ALL-INKL.COM's storage without asking anything
// of the network beyond port 22. Their Netzlaufwerk is an SMB share, and the
// space behind it is the same space the webspace uses -- so an SSH user
// (`ssh-w0XXXXX@w0XXXXX.kasserver.com`, enabled in the KAS under Tools -> SSH)
// sees the same files, over a port that a hosted container is actually allowed
// to open.
//
// Authentication is a password or a private key. A key is the better of the
// two for something that runs unattended, and it is what the hoster's own
// public-key instructions set up.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type sftpStore struct {
	client *sftp.Client
	ssh    *ssh.Client
	root   string
}

var _ remoteStore = (*sftpStore)(nil)

func newSFTPStore(ctx context.Context, address targetAddress, username, password, privateKey string) (remoteStore, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("an SFTP target needs a user name")
	}
	auth, err := sshAuthMethods(password, privateKey)
	if err != nil {
		return nil, err
	}
	conn, err := safeDialContext(ctx, "tcp", net.JoinHostPort(address.Host, fmt.Sprint(address.Port)))
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: username,
		Auth: auth,
		// The host key is not pinned, and that is a deliberate, stated limit
		// rather than an oversight: this plugin has nowhere to keep a
		// known_hosts file per user, and refusing every first connection would
		// mean nobody could configure a target at all. The connection is still
		// encrypted, and the credential sent over it is only ever this target's
		// own -- but a network attacker positioned between this server and the
		// hoster could impersonate the host. An install that cannot accept that
		// should use the WebDAV transport over TLS, where the certificate chain
		// is checked.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, address.Host, config)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the SFTP server refused the connection: %w", err)
	}
	// The dial deadline must not outlive the handshake, or every later read on
	// this connection inherits it and the session dies mid-upload.
	_ = conn.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(clientConn, channels, requests)
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("the SFTP session could not be opened: %w", err)
	}
	return &sftpStore{client: client, ssh: sshClient, root: address.Root}, nil
}

// sshAuthMethods turns what the target stored into what the SSH handshake
// takes. A key and a password may both be present, in which case the key is
// offered first and the password is the fallback the server may still ask for.
func sshAuthMethods(password, privateKey string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(privateKey); key != "" {
		signer, err := parsePrivateKey(key, password)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if password != "" {
		methods = append(methods, ssh.Password(password))
		// Some servers ask for the password through keyboard-interactive rather
		// than the password method, and answer the password method with a
		// refusal; answering both is what makes one stored password work
		// against either.
		methods = append(methods, ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}))
	}
	if len(methods) == 0 {
		return nil, errors.New("an SFTP target needs a password or a private key")
	}
	return methods, nil
}

// parsePrivateKey reads a stored key, trying it unencrypted first so a
// passphrase-free key does not need the password field filled in.
func parsePrivateKey(key, passphrase string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) && passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
		if err == nil {
			return signer, nil
		}
		return nil, errors.New("the private key could not be read with the stored password as its passphrase")
	}
	return nil, errors.New("the private key could not be read")
}

func (s *sftpStore) Close() error {
	err := s.client.Close()
	if sshErr := s.ssh.Close(); err == nil {
		err = sshErr
	}
	return err
}

func (s *sftpStore) resolve(relative string) string {
	return "/" + joinRoot(s.root, relative)
}

func (s *sftpStore) CheckAccess(ctx context.Context) error {
	_, err := s.List(ctx, "")
	return err
}

func (s *sftpStore) List(ctx context.Context, dir string) ([]resourceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := s.resolve(dir)
	infos, err := s.client.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("reading %q over SFTP failed: %w", dir, err)
	}
	base := strings.Trim(cleanRemotePath(dir), "/")
	out := make([]resourceEntry, 0, len(infos))
	for _, info := range infos {
		name := info.Name()
		if name == "." || name == ".." {
			continue
		}
		entry := resourceEntry{
			Name:       name,
			Path:       name,
			IsDir:      info.IsDir(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC().Unix(),
		}
		if base != "" {
			entry.Path = base + "/" + name
		}
		if entry.IsDir {
			entry.Path += "/"
			entry.Size = 0
		} else {
			entry.ContentType = guessContentType(name)
		}
		out = append(out, entry)
	}
	// A directory listing arrives in whatever order the server walked it, and a
	// file browser that reshuffles between refreshes is unusable.
	sortEntries(out)
	return out, nil
}

func (s *sftpStore) Exists(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := s.client.Stat(s.resolve(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("looking up %q over SFTP failed: %w", name, err)
	}
	return true, nil
}

func (s *sftpStore) Put(ctx context.Context, name string, data []byte, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := s.resolve(name)
	if err := s.client.MkdirAll(path.Dir(target)); err != nil {
		return fmt.Errorf("creating %q over SFTP failed: %w", path.Dir(name), err)
	}
	file, err := s.client.Create(target)
	if err != nil {
		return fmt.Errorf("creating %q over SFTP failed: %w", name, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing %q over SFTP failed: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %q over SFTP failed: %w", name, err)
	}
	return nil
}

func (s *sftpStore) Get(ctx context.Context, name string) (io.ReadCloser, string, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", 0, err
	}
	target := s.resolve(name)
	info, err := s.client.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", 0, errNotFound
		}
		return nil, "", 0, fmt.Errorf("looking up %q over SFTP failed: %w", name, err)
	}
	if info.IsDir() {
		return nil, "", 0, errors.New("that path is a folder, not a file")
	}
	if info.Size() > maxDownloadBytes {
		return nil, "", 0, errors.New("the file is larger than this proxy will serve")
	}
	file, err := s.client.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", 0, errNotFound
		}
		return nil, "", 0, fmt.Errorf("opening %q over SFTP failed: %w", name, err)
	}
	return newLimitedReadCloser(file, maxDownloadBytes), guessContentType(name), info.Size(), nil
}

func (s *sftpStore) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.client.Remove(s.resolve(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return fmt.Errorf("deleting %q over SFTP failed: %w", name, err)
	}
	return nil
}

// sortEntries puts folders first and then orders by name, which is the order a
// file browser is read in.
func sortEntries(entries []resourceEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}
