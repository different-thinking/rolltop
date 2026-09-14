// File overview: The one shape the rest of the plugin files things through, and
// the three transports behind it.
//
// The queue, the worker, the browser and the settings page do not know which
// protocol a target speaks. They ask for a store, put bytes in it, list it and
// read back -- so adding a transport is a file, not a change to how any of that
// works.
//
// Which transport a target uses is decided by the scheme of its address rather
// than by a separate setting, because the address already says it and two
// fields that can disagree are a support question waiting to happen:
//
//	https://cloud.example.org/remote.php/dav/files/me/Recordings/
//	sftp://w0123456.kasserver.com/recordings
//	smb://s0123456.kasserver.com/s0123456/recordings
//
// The last two are the shapes ALL-INKL.COM's own instructions hand out for the
// Netzlaufwerk, which is an SMB share and not a WebDAV service -- pasting what
// the hoster shows is meant to be enough.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"strings"
)

// remoteStore is one configured destination, opened. Every method takes a path
// relative to the target's own root; none of them can address anything above
// it.
type remoteStore interface {
	// CheckAccess answers whether the destination is reachable and the
	// credentials are accepted, which is what the settings page's Test asks.
	CheckAccess(ctx context.Context) error
	List(ctx context.Context, dir string) ([]resourceEntry, error)
	Exists(ctx context.Context, name string) (bool, error)
	// Put writes one file, creating the directories above it. contentType is
	// advisory: WebDAV sends it, a filesystem has nowhere to keep it.
	Put(ctx context.Context, name string, data []byte, contentType string) error
	// Get streams one file back. The caller closes the reader. A transport with
	// no notion of content type returns "" and leaves the guess to the caller.
	Get(ctx context.Context, name string) (io.ReadCloser, string, int64, error)
	Delete(ctx context.Context, name string) error
	// Close releases the connection. WebDAV is stateless and has none; SFTP and
	// SMB hold one, and a caller that forgets leaks it.
	Close() error
}

// The transports a target address can name.
const (
	transportWebDAV = "webdav"
	transportSFTP   = "sftp"
	transportSMB    = "smb"
)

// targetAddress is a parsed destination: which transport, where, and the path
// under it everything is filed beneath.
type targetAddress struct {
	Transport string
	// URL is the address as stored, normalized. It is what the settings page
	// shows back and what the browser builds links from.
	URL  *url.URL
	Host string
	Port int
	// Share is the SMB share name, which is the first path segment of an
	// `smb://` address and empty for the others.
	Share string
	// Root is the directory inside the destination everything is filed under.
	Root string
}

// defaultPorts are what an address that names none is dialed on.
var defaultPorts = map[string]int{
	transportWebDAV: 0, // decided by the http scheme
	transportSFTP:   22,
	transportSMB:    445,
}

// parseTargetAddress reads one configured address. It is the only place an
// address is judged, so the settings form, the worker and the browser cannot
// disagree about what is acceptable.
func parseTargetAddress(raw string) (targetAddress, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return targetAddress{}, errors.New("an address is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return targetAddress{}, fmt.Errorf("the address could not be read: %w", err)
	}
	if parsed.Hostname() == "" {
		return targetAddress{}, errors.New("the address has no host")
	}
	if parsed.User != nil {
		return targetAddress{}, errors.New("put the user name in its own field, not in the address")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""

	out := targetAddress{Host: parsed.Hostname()}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http":
		out.Transport = transportWebDAV
	case "sftp", "ssh":
		out.Transport = transportSFTP
		parsed.Scheme = "sftp"
	case "smb", "cifs":
		out.Transport = transportSMB
		parsed.Scheme = "smb"
	case "":
		return targetAddress{}, errors.New("the address must start with https://, sftp:// or smb://")
	default:
		return targetAddress{}, fmt.Errorf("this plugin cannot speak %q; use https://, sftp:// or smb://", parsed.Scheme)
	}

	out.Port = defaultPorts[out.Transport]
	if raw := parsed.Port(); raw != "" {
		port, err := parsePort(raw)
		if err != nil {
			return targetAddress{}, err
		}
		out.Port = port
	}

	cleaned := cleanRemotePath(parsed.Path)
	if out.Transport == transportSMB {
		// An SMB address names a share before it names a directory, and a share
		// is not optional: there is nothing to open without one.
		share, rest, _ := strings.Cut(strings.TrimSuffix(cleaned, "/"), "/")
		if share == "" {
			return targetAddress{}, errors.New("an smb:// address needs a share, as in smb://host/share/folder")
		}
		out.Share = share
		out.Root = rest
	} else {
		out.Root = strings.TrimSuffix(cleaned, "/")
	}

	// The base is a collection, so it ends in a slash: resolving a relative
	// path against a base without one drops its last segment.
	parsed.Path = "/" + cleaned
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	out.URL = parsed
	return out, nil
}

func parsePort(raw string) (int, error) {
	port := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("the port %q is not a number", raw)
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return 0, fmt.Errorf("the port %q is out of range", raw)
		}
	}
	if port == 0 {
		return 0, errors.New("the port may not be zero")
	}
	return port, nil
}

// openRemoteStore opens the destination one target names. The credentials are
// passed in already decrypted, because deciding who may read them belongs to
// the caller rather than here.
func openRemoteStore(ctx context.Context, item target, password, privateKey string) (remoteStore, error) {
	address, err := parseTargetAddress(item.BaseURL)
	if err != nil {
		return nil, err
	}
	switch address.Transport {
	case transportWebDAV:
		return newWebDAVClient(address, item.Username, password)
	case transportSFTP:
		return newSFTPStore(ctx, address, item.Username, password, privateKey)
	case transportSMB:
		return newSMBStore(ctx, address, item.Username, password)
	}
	return nil, fmt.Errorf("unknown transport %q", address.Transport)
}

// joinRoot puts a caller's relative path under the target's own root. Both
// halves are cleaned first, so nothing a caller passes can climb above it.
func joinRoot(root, relative string) string {
	cleaned := cleanRemotePath(relative)
	root = strings.Trim(cleanRemotePath(root), "/")
	if root == "" {
		return cleaned
	}
	if cleaned == "" {
		return root + "/"
	}
	return root + "/" + cleaned
}

// guessContentType names a file by its extension, for the transports that keep
// no content type of their own. A filesystem stores bytes and a name; the type
// a browser needs to play a recording in place has to come from somewhere, and
// the name is what there is.
//
// The audio table below is consulted before the system's, deliberately. The
// system answer comes from /etc/mime.types, which differs between a developer's
// machine and the scratch container this ships in -- one has no entry for
// `.m4a` at all, another calls `.amr` `audio/AMR`, another maps `.opus` to
// `audio/ogg`. For the handful of formats this archive is actually full of,
// answering the same way everywhere is worth more than deferring to whatever
// the image happens to carry. Everything else is the system's to name.
func guessContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return ""
	}
	if known, ok := audioContentTypes[ext]; ok {
		return known
	}
	return mime.TypeByExtension(ext)
}

// audioContentTypes is the recording formats a phone or a voice recorder
// produces, named the way the browsers that have to play them expect.
var audioContentTypes = map[string]string{
	".m4a":  "audio/mp4",
	".m4b":  "audio/mp4",
	".mp3":  "audio/mpeg",
	".opus": "audio/opus",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".aac":  "audio/aac",
	".amr":  "audio/amr",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".wma":  "audio/x-ms-wma",
}
