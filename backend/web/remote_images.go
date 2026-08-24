// File overview: Remote email image cache integration for message rendering.

package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/remoteimages"
	"rolltop/backend/store"
)

// cachedRemoteImageURLs maps the remote images of a body onto the local cache,
// for the ones already in it. A blocked image is deliberately left out of that
// map: the block rules are written against the URL the sender wrote, and
// removeBlockedRemoteImages recognises the tag to drop by exactly that URL, so
// an image rewritten to /remote-images/<hash> would survive every rule - among
// them a rule added after the image was cached, which would then never match
// again for as long as the cache entry lives.
func (s *Server) cachedRemoteImageURLs(ctx context.Context, userID int64, msg store.MessageRecord, bodyHTML string, blockRules []string) map[string]string {
	if s == nil || s.store == nil || s.blobs == nil || strings.TrimSpace(bodyHTML) == "" {
		return nil
	}
	candidates := remoteimages.Extract(bodyHTML)
	if len(candidates) == 0 {
		return nil
	}
	blockers := compileRemoteImageBlockPatterns(blockRules)
	out := map[string]string{}
	var missing []remoteimages.Candidate
	for _, candidate := range candidates {
		if remoteImageURLBlocked(blockers, candidate.URL) {
			continue
		}
		hash := remoteimages.Hash(candidate.URL)
		cache, err := s.store.GetRemoteImageCacheByHash(ctx, userID, hash)
		if err == nil && cache.Status == store.RemoteImageStatusOK && strings.TrimSpace(cache.BlobPath) != "" {
			out[hash] = remoteimages.CachedURL(hash)
			continue
		}
		if err != nil || !store.RemoteImageCacheFresh(cache, time.Now().Unix()) {
			missing = append(missing, candidate)
		}
	}
	if len(missing) > 0 {
		// Warming is handed the candidates this loop kept rather than the body,
		// so a blocked image is not fetched on the way to being dropped.
		s.remoteImageCache().WarmCandidatesAsync(plugins.RemoteImageFetchRequest{
			UserID:    userID,
			MessageID: msg.MessageIDHeader,
			Sender:    msg.FromAddr,
		}, missing)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) remoteImageCache() remoteimages.Cache {
	return remoteimages.Cache{
		Store: s.store,
		Blobs: s.blobs,
		Allow: func(ctx context.Context, req plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error) {
			return s.allowRemoteImageFetch(ctx, req)
		},
	}
}

func (s *Server) allowRemoteImageFetch(ctx context.Context, req plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error) {
	if s == nil || s.store == nil || !s.pluginEnabled(ctx, plugins.RemoteImageBlocklist) {
		return plugins.RemoteImageFetchDecision{Allow: true}, nil
	}
	hook, ok := remoteImageBlocklistHook()
	if !ok {
		return plugins.RemoteImageFetchDecision{Allow: true}, nil
	}
	return hook.AllowRemoteImageFetch(ctx, s.store.DB(), req)
}

func (s *Server) handleRemoteImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	hash := strings.Trim(strings.TrimPrefix(r.URL.Path, remoteimages.CachedURLPrefix), "/")
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	cache, err := s.store.GetRemoteImageCacheByHash(r.Context(), cu.User.ID, hash)
	if err != nil {
		// The lookup error has to be classified before the cache fields are
		// read: on a store failure the zero-valued cache also fails the checks
		// below, which turned every such failure into a silent 404.
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if cache.Status != store.RemoteImageStatusOK || strings.TrimSpace(cache.BlobPath) == "" {
		http.NotFound(w, r)
		return
	}
	file, err := s.blobs.OpenUserBlob(cu.User.ID, cache.BlobPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	contentType := strings.TrimSpace(cache.ContentType)
	if contentType == "" {
		contentType = "image/*"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Vary", "Cookie")
	http.ServeContent(w, r, hash, cache.FetchedAt, file)
}
