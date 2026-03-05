package r2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire(t *testing.T) {
	const (
		accountID = "test-account-123"
		tokenID   = "tok-abc"
		tokenVal  = "secret-token-value"
	)

	var (
		bucketCreated   atomic.Bool
		tokenCreated    atomic.Bool
		permGroupCalled atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Create bucket
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			bucketCreated.Store(true)
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "tscloudvpn-vol-testvolume", body["name"])
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"name": body["name"]},
			})

		// List tokens (for revocation)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []any{},
			})

		// Permission groups
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permission_groups"):
			permGroupCalled.Store(true)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "pg-other", "name": "Workers R2 Storage Read"},
					{"id": "pg-write", "name": "Workers R2 Storage Bucket Item Write"},
				},
			})

		// Create token
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			tokenCreated.Store(true)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "tscloudvpn-vol-testvolume", body["name"])

			policies := body["policies"].([]any)
			policy := policies[0].(map[string]any)
			permGroups := policy["permission_groups"].([]any)
			pg := permGroups[0].(map[string]any)
			assert.Equal(t, "pg-write", pg["id"])

			resources := policy["resources"].(map[string]any)
			expectedResource := "com.cloudflare.edge.r2.bucket." + accountID + "_default_tscloudvpn-vol-testvolume"
			assert.Contains(t, resources, expectedResource)

			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]string{
					"id":    tokenID,
					"value": tokenVal,
				},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tm := NewTokenManager(accountID, "master-token")
	tm.client = srv.Client()
	// Override API base to point to test server
	origBase := cfAPIBase
	_ = origBase // cfAPIBase is a const, so we override via the doRequest method
	// We need to make the TokenManager use the test server URL. Let's set it via a custom HTTP client transport.
	tm.client = &http.Client{
		Transport: &rewriteTransport{base: srv.URL},
	}

	creds, err := tm.Acquire(context.Background(), "testvolume", "test-vm")
	require.NoError(t, err)
	require.NotNil(t, creds)

	assert.True(t, bucketCreated.Load(), "bucket should be created")
	assert.True(t, tokenCreated.Load(), "token should be created")
	assert.True(t, permGroupCalled.Load(), "permission groups should be looked up")

	assert.Equal(t, tokenID, creds.AccessKeyID)
	expectedSecret := sha256.Sum256([]byte(tokenVal))
	assert.Equal(t, hex.EncodeToString(expectedSecret[:]), creds.SecretAccessKey)
	assert.Equal(t, "https://"+accountID+".r2.cloudflarestorage.com", creds.Endpoint)
	assert.Equal(t, "tscloudvpn-vol-testvolume", creds.Bucket)
}

func TestAcquireRevokesExistingToken(t *testing.T) {
	const accountID = "test-account-456"

	var tokenDeleted atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{}})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "old-token-id", "name": "tscloudvpn-vol-myvol"},
					{"id": "other-token", "name": "unrelated-token"},
				},
			})

		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/tokens/old-token-id"):
			tokenDeleted.Store(true)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permission_groups"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "pg-write", "name": "Workers R2 Storage Bucket Item Write"},
				},
			})

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"id": "new-tok", "value": "new-secret"},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tm := NewTokenManager(accountID, "master-token")
	tm.client = &http.Client{Transport: &rewriteTransport{base: srv.URL}}

	creds, err := tm.Acquire(context.Background(), "myvol", "vm-1")
	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.True(t, tokenDeleted.Load(), "existing token should be revoked before creating new one")
	assert.Equal(t, "new-tok", creds.AccessKeyID)
}

func TestAcquireBucketAlreadyExists(t *testing.T) {
	const accountID = "test-account-789"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "bucket already exists"}}})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permission_groups"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []map[string]string{{"id": "pg-write", "name": "Workers R2 Storage Bucket Item Write"}},
			})

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"id": "tok-1", "value": "val-1"},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tm := NewTokenManager(accountID, "master-token")
	tm.client = &http.Client{Transport: &rewriteTransport{base: srv.URL}}

	creds, err := tm.Acquire(context.Background(), "existing", "vm-2")
	require.NoError(t, err)
	assert.Equal(t, "tok-1", creds.AccessKeyID)
}

func TestRelease(t *testing.T) {
	const accountID = "test-account-rel"

	var tokenDeleted atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]string{
					{"id": "active-tok", "name": "tscloudvpn-vol-releaseme"},
				},
			})

		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/tokens/active-tok"):
			tokenDeleted.Store(true)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tm := NewTokenManager(accountID, "master-token")
	tm.client = &http.Client{Transport: &rewriteTransport{base: srv.URL}}

	err := tm.Release(context.Background(), "releaseme")
	require.NoError(t, err)
	assert.True(t, tokenDeleted.Load(), "token should be deleted")
}

func TestPermissionGroupCaching(t *testing.T) {
	const accountID = "test-account-cache"

	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permission_groups"):
			callCount.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []map[string]string{{"id": "pg-write", "name": "Workers R2 Storage Bucket Item Write"}},
			})

		default:
			// Ignore other requests for this test
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		}
	}))
	defer srv.Close()

	tm := NewTokenManager(accountID, "master-token")
	tm.client = &http.Client{Transport: &rewriteTransport{base: srv.URL}}

	ctx := context.Background()
	id1, err := tm.getBucketItemWriteGroupID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pg-write", id1)

	id2, err := tm.getBucketItemWriteGroupID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pg-write", id2)

	assert.Equal(t, int32(1), callCount.Load(), "permission groups should only be fetched once")
}

// rewriteTransport rewrites requests from the Cloudflare API base URL
// to the test server URL, preserving the path.
type rewriteTransport struct {
	base string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(req)
}
