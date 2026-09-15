// File overview: The SMB transport -- the protocol ALL-INKL.COM's Netzlaufwerk
// actually speaks.
//
// Their own instructions hand out `smb://s0123456.kasserver.com/s0123456`, and
// their support says plainly that the network drive is not a WebDAV service. So
// a target pointed at it needs SMB, and this is it.
//
// Two things to know before choosing this transport over SFTP:
//
//   - Port 445 is blocked outbound by a great many hosters and cloud networks,
//     because it is a decades-old worm vector. ALL-INKL.COM offering a VPN for
//     the network drive is a symptom of exactly that. If the server running
//     Rolltop cannot open 445 to the hoster, no amount of correctness here
//     helps, and SFTP -- which reaches the same files on the same storage over
//     port 22 -- is the way in.
//   - github.com/hirochachacha/go-smb2 is the pure-Go SMB2/3 client everything
//     in this ecosystem uses, and its last release was March 2022. It works and
//     it is widely deployed; it is not actively maintained. That is a real cost
//     of this transport and the reason the settings page names SFTP first.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"

	"github.com/hirochachacha/go-smb2"
)

type smbStore struct {
	session *smb2.Session
	share   *smb2.Share
	conn    net.Conn
	root    string
}

var _ remoteStore = (*smbStore)(nil)

func newSMBStore(ctx context.Context, address targetAddress, username, password string) (remoteStore, error) {
	if strings.TrimSpace(address.Share) == "" {
		return nil, errors.New("an smb:// address needs a share, as in smb://host/share/folder")
	}
	conn, err := safeDialContext(ctx, "tcp", net.JoinHostPort(address.Host, fmt.Sprint(address.Port)))
	if err != nil {
		return nil, err
	}
	domain, username := splitSMBUser(username)
	dialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{User: username, Password: password, Domain: domain},
	}
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the SMB server refused the connection: %w", err)
	}
	share, err := session.Mount(address.Share)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, fmt.Errorf("the share %q could not be opened: %w", address.Share, err)
	}
	return &smbStore{session: session, share: share, conn: conn, root: address.Root}, nil
}

// splitSMBUser separates a domain from a user name. A Windows share is usually
// written down as `DOMAIN\user`, and that is what a reader will paste into the
// user field -- so it is read here rather than made into a third form field
// nobody outside a Windows network would fill in.
func splitSMBUser(username string) (string, string) {
	if index := strings.IndexAny(username, `\/`); index >= 0 {
		return username[:index], username[index+1:]
	}
	return "", username
}

func (s *smbStore) Close() error {
	err := s.share.Umount()
	if logoffErr := s.session.Logoff(); err == nil {
		err = logoffErr
	}
	if connErr := s.conn.Close(); err == nil {
		err = connErr
	}
	return err
}

// resolve is relative to the mounted share, so it carries no leading slash.
func (s *smbStore) resolve(relative string) string {
	joined := joinRoot(s.root, relative)
	return strings.Trim(joined, "/")
}

func (s *smbStore) CheckAccess(ctx context.Context) error {
	_, err := s.List(ctx, "")
	return err
}

func (s *smbStore) List(ctx context.Context, dir string) ([]resourceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := s.resolve(dir)
	if target == "" {
		target = "."
	}
	infos, err := s.share.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("reading %q over SMB failed: %w", dir, err)
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
	sortEntries(out)
	return out, nil
}

func (s *smbStore) Exists(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := s.share.Stat(s.resolve(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("looking up %q over SMB failed: %w", name, err)
	}
	return true, nil
}

func (s *smbStore) Put(ctx context.Context, name string, data []byte, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := s.resolve(name)
	if dir := path.Dir(target); dir != "." && dir != "/" {
		if err := s.share.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %q over SMB failed: %w", dir, err)
		}
	}
	file, err := s.share.Create(target)
	if err != nil {
		return fmt.Errorf("creating %q over SMB failed: %w", name, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing %q over SMB failed: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %q over SMB failed: %w", name, err)
	}
	return nil
}

func (s *smbStore) Get(ctx context.Context, name string) (io.ReadCloser, string, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", 0, err
	}
	target := s.resolve(name)
	info, err := s.share.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", 0, errNotFound
		}
		return nil, "", 0, fmt.Errorf("looking up %q over SMB failed: %w", name, err)
	}
	if info.IsDir() {
		return nil, "", 0, errors.New("that path is a folder, not a file")
	}
	if info.Size() > maxDownloadBytes {
		return nil, "", 0, errTooLarge
	}
	file, err := s.share.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", 0, errNotFound
		}
		return nil, "", 0, fmt.Errorf("opening %q over SMB failed: %w", name, err)
	}
	return newLimitedReadCloser(file, maxDownloadBytes), guessContentType(name), info.Size(), nil
}

func (s *smbStore) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.share.Remove(s.resolve(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return fmt.Errorf("deleting %q over SMB failed: %w", name, err)
	}
	return nil
}
