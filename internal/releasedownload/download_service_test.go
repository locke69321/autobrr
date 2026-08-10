// Copyright (c) 2021-2025, Ludvig Lundgren and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package releasedownload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobrr/autobrr/internal/domain"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

const testTorrent = "d8:announce0:4:infod4:name4:test6:lengthi1eee"

type mockIndexerRepo struct {
	settings map[string]string
	stored   []map[string]string
}

func (m *mockIndexerRepo) FindByID(_ context.Context, id int) (*domain.Indexer, error) {
	return &domain.Indexer{ID: int64(id), Identifier: "mock-feed", Settings: m.settings}, nil
}

func (m *mockIndexerRepo) UpdateSettings(_ context.Context, _ int64, settings map[string]string) error {
	m.stored = append(m.stored, settings)

	return nil
}

type mockProxyService struct{}

func (m *mockProxyService) FindByID(_ context.Context, id int64) (*domain.Proxy, error) {
	return &domain.Proxy{ID: id}, nil
}

func TestRotateRawCookie(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		setCookie []string
		want      string
	}{
		{
			name:      "rotated value is picked up",
			raw:       "mam_id=old",
			setCookie: []string{"mam_id=new; Path=/; Max-Age=1296000; Secure"},
			want:      "mam_id=new",
		},
		{
			name:      "a documented trailing semicolon is kept",
			raw:       "mam_id=old;",
			setCookie: []string{"mam_id=new"},
			want:      "mam_id=new;",
		},
		{
			name:      "cookies we do not already hold are ignored",
			raw:       "mam_id=old",
			setCookie: []string{"cf_clearance=bot-check", "__cf_bm=fingerprint"},
			want:      "",
		},
		{
			name:      "an expiring cookie does not blank the stored value",
			raw:       "mam_id=old",
			setCookie: []string{"mam_id=; Max-Age=0"},
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			for _, c := range tt.setCookie {
				resp.Header.Add("Set-Cookie", c)
			}

			assert.Equal(t, tt.want, rotateRawCookie(tt.raw, resp))
		})
	}
}

// rotatingDownload serves body with setCookie for a rotating indexer holding
// repo's cookie. The returned pointer receives the Cookie header the tracker saw.
func rotatingDownload(t *testing.T, repo *mockIndexerRepo, setCookie, body string) (*DownloadService, *domain.Release, *string) {
	t.Helper()

	sent := new(string)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sent = r.Header.Get("Cookie")
		w.Header().Set("Set-Cookie", setCookie)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	rls := domain.NewRelease(domain.IndexerMinimal{ID: 1, Name: "Mock Indexer", Identifier: "mock-indexer"})
	rls.Protocol = domain.ReleaseProtocolTorrent
	rls.DownloadURL = srv.URL + "/tor/download.php?tid=1"
	rls.RawCookie = repo.settings["cookie"]
	rls.RotateCookie = true

	return NewDownloadService(zerolog.New(io.Discard), repo, &mockProxyService{}), rls, sent
}

// The cookie stored on the indexer is sent rather than the one copied onto the
// release when the announce was parsed, and the value it comes back with is
// stored in turn.
func TestDownloadService_CookieRotation(t *testing.T) {
	repo := &mockIndexerRepo{settings: map[string]string{"cookie": "mam_id=stored"}}
	svc, rls, sent := rotatingDownload(t, repo, "mam_id=rotated; Max-Age=1296000", testTorrent)
	rls.RawCookie = "mam_id=stale"

	assert.NoError(t, svc.DownloadRelease(t.Context(), rls))

	assert.Equal(t, "mam_id=stored", *sent)
	assert.Len(t, repo.stored, 1)
	assert.Equal(t, "mam_id=rotated", repo.stored[0]["cookie"])
}

// An indexer that has not opted in keeps the announce-time cookie and is never
// written to.
func TestDownloadService_CookieRotationOptOut(t *testing.T) {
	repo := &mockIndexerRepo{settings: map[string]string{"cookie": "mam_id=stored"}}
	svc, rls, sent := rotatingDownload(t, repo, "mam_id=rotated", testTorrent)
	rls.RawCookie = "mam_id=snapshot"
	rls.RotateCookie = false

	assert.NoError(t, svc.DownloadRelease(t.Context(), rls))

	assert.Equal(t, "mam_id=snapshot", *sent)
	assert.Empty(t, repo.stored)
}

// Trackers serve a login page as 200 when a session lapses. Nothing that fails
// to decode as a torrent may replace a working cookie.
func TestDownloadService_CookieRotationIgnoresNonTorrentBody(t *testing.T) {
	repo := &mockIndexerRepo{settings: map[string]string{"cookie": "mam_id=stored"}}
	svc, rls, _ := rotatingDownload(t, repo, "mam_id=issued-by-login-page", "<html><body>Please log in</body></html>")

	assert.Error(t, svc.DownloadRelease(t.Context(), rls))
	assert.Empty(t, repo.stored, "a non-torrent body must not rotate the stored cookie")
}

func TestDownloadService_ResolveMagnetURI(t *testing.T) {
	const magnetURI = "magnet:?xt=urn:btih:deadbeef"

	tests := []struct {
		name    string
		handler http.HandlerFunc
		// magnetURI on the release before resolving, %s is replaced with the test server url
		before string
		want   string
	}{
		{
			name:   "empty is left alone",
			before: "",
			want:   "",
		},
		{
			name:   "magnet is left alone",
			before: magnetURI,
			want:   magnetURI,
		},
		{
			name: "redirect to a magnet is followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, magnetURI, http.StatusFound)
			},
			before: "%s/dl/mock",
			want:   magnetURI,
		},
		{
			name: "magnet in the body is used",
			handler: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, magnetURI+"\n")
			},
			before: "%s/dl/mock",
			want:   magnetURI,
		},
		{
			name: "a response that is not a magnet is discarded",
			handler: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "d8:announce20:https://fake-feed.come")
			},
			before: "%s/dl/mock",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			magnet := tt.before

			if tt.handler != nil {
				srv := httptest.NewServer(tt.handler)
				defer srv.Close()

				magnet = strings.Replace(tt.before, "%s", srv.URL, 1)
			}

			svc := NewDownloadService(zerolog.New(io.Discard), &mockIndexerRepo{}, &mockProxyService{})

			rls := domain.NewRelease(domain.IndexerMinimal{ID: 1, Name: "Mock Feed", Identifier: "mock-feed"})
			rls.MagnetURI = magnet

			err := svc.ResolveMagnetURI(t.Context(), rls)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, rls.MagnetURI)
		})
	}
}
