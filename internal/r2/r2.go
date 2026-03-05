// Package r2 manages Cloudflare R2 buckets and scoped S3 tokens for persistent volumes.
//
// Each volume gets its own R2 bucket (tscloudvpn-vol-{name}) for least-privilege
// isolation. A compromised VM can only access its own volume's data.
//
// S3 credentials are derived from Cloudflare API tokens:
//   - Access Key ID = token ID
//   - Secret Access Key = SHA-256(token value)
package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const (
	cfAPIBase = "https://api.cloudflare.com/client/v4"

	// tokenNamePrefix is the naming convention for volume tokens,
	// used to discover and revoke them.
	tokenNamePrefix = "tscloudvpn-vol-"

	// bucketNamePrefix is the naming convention for volume buckets.
	bucketNamePrefix = "tscloudvpn-vol-"

	// bucketItemWritePermission is the name of the Cloudflare permission group
	// that grants object read/write on a single R2 bucket.
	bucketItemWritePermission = "Workers R2 Storage Bucket Item Write"
)

// S3Credentials holds the credentials a VM needs to access its R2 bucket via S3.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey  string
	Endpoint        string // https://{account_id}.r2.cloudflarestorage.com
	Bucket          string
}

// TokenManager manages R2 buckets and scoped S3 tokens for persistent volumes.
type TokenManager struct {
	accountID string
	apiToken  string
	client    *http.Client

	mu                    sync.Mutex
	bucketItemWriteGroupID string // cached permission group ID
}

// NewTokenManager creates a new TokenManager.
func NewTokenManager(accountID, apiToken string) *TokenManager {
	return &TokenManager{
		accountID: accountID,
		apiToken:  apiToken,
		client:    &http.Client{},
	}
}

// Acquire ensures the volume's R2 bucket exists, revokes any existing token,
// creates a new scoped S3 token, and returns the credentials.
func (tm *TokenManager) Acquire(ctx context.Context, volumeName, vmHostname string) (*S3Credentials, error) {
	bucketName := bucketNamePrefix + volumeName
	tokenName := tokenNamePrefix + volumeName

	// Step 1: Ensure bucket exists
	if err := tm.ensureBucket(ctx, bucketName); err != nil {
		return nil, fmt.Errorf("ensure bucket %s: %w", bucketName, err)
	}

	// Step 2: Revoke any existing token for this volume
	if err := tm.revokeTokenByName(ctx, tokenName); err != nil {
		return nil, fmt.Errorf("revoke existing token for %s: %w", volumeName, err)
	}

	// Step 3: Create a new scoped token
	creds, err := tm.createScopedToken(ctx, tokenName, bucketName)
	if err != nil {
		return nil, fmt.Errorf("create token for %s: %w", volumeName, err)
	}

	return creds, nil
}

// Release revokes the active S3 token for the given volume (fencing).
func (tm *TokenManager) Release(ctx context.Context, volumeName string) error {
	tokenName := tokenNamePrefix + volumeName
	return tm.revokeTokenByName(ctx, tokenName)
}

// ensureBucket creates the R2 bucket if it doesn't already exist.
func (tm *TokenManager) ensureBucket(ctx context.Context, bucketName string) error {
	body, err := json.Marshal(map[string]string{"name": bucketName})
	if err != nil {
		return err
	}

	resp, err := tm.doRequest(ctx, "POST", fmt.Sprintf("/accounts/%s/r2/buckets", tm.accountID), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 409 means bucket already exists — that's fine
	if resp.StatusCode == http.StatusConflict {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		return tm.apiError("create bucket", resp)
	}

	return nil
}

// revokeTokenByName finds and deletes any Cloudflare API token matching the given name.
func (tm *TokenManager) revokeTokenByName(ctx context.Context, tokenName string) error {
	// List all tokens for this account
	resp, err := tm.doRequest(ctx, "GET", fmt.Sprintf("/accounts/%s/tokens", tm.accountID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return tm.apiError("list tokens", resp)
	}

	var listResp struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return fmt.Errorf("decode token list: %w", err)
	}

	for _, token := range listResp.Result {
		if token.Name == tokenName {
			delResp, err := tm.doRequest(ctx, "DELETE", fmt.Sprintf("/accounts/%s/tokens/%s", tm.accountID, token.ID), nil)
			if err != nil {
				return err
			}
			delResp.Body.Close()

			if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("delete token %s: HTTP %d", token.ID, delResp.StatusCode)
			}
		}
	}

	return nil
}

// createScopedToken creates a Cloudflare API token scoped to a single R2 bucket
// and returns the derived S3 credentials.
func (tm *TokenManager) createScopedToken(ctx context.Context, tokenName, bucketName string) (*S3Credentials, error) {
	permGroupID, err := tm.getBucketItemWriteGroupID(ctx)
	if err != nil {
		return nil, err
	}

	resource := fmt.Sprintf("com.cloudflare.edge.r2.bucket.%s_default_%s", tm.accountID, bucketName)

	reqBody := map[string]any{
		"name": tokenName,
		"policies": []map[string]any{
			{
				"effect": "allow",
				"permission_groups": []map[string]string{
					{"id": permGroupID},
				},
				"resources": map[string]string{
					resource: "*",
				},
			},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := tm.doRequest(ctx, "POST", fmt.Sprintf("/accounts/%s/tokens", tm.accountID), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, tm.apiError("create token", resp)
	}

	var createResp struct {
		Result struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("decode create token response: %w", err)
	}

	if createResp.Result.Value == "" {
		return nil, fmt.Errorf("token created but no value returned")
	}

	// S3 credentials: Access Key ID = token ID, Secret = SHA-256(token value)
	hash := sha256.Sum256([]byte(createResp.Result.Value))

	return &S3Credentials{
		AccessKeyID:    createResp.Result.ID,
		SecretAccessKey: hex.EncodeToString(hash[:]),
		Endpoint:       fmt.Sprintf("https://%s.r2.cloudflarestorage.com", tm.accountID),
		Bucket:         bucketName,
	}, nil
}

// getBucketItemWriteGroupID returns the permission group UUID for
// "Workers R2 Storage Bucket Item Write", discovering it on first call.
func (tm *TokenManager) getBucketItemWriteGroupID(ctx context.Context) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.bucketItemWriteGroupID != "" {
		return tm.bucketItemWriteGroupID, nil
	}

	resp, err := tm.doRequest(ctx, "GET", "/user/tokens/permission_groups", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", tm.apiError("list permission groups", resp)
	}

	var pgResp struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pgResp); err != nil {
		return "", fmt.Errorf("decode permission groups: %w", err)
	}

	for _, pg := range pgResp.Result {
		if pg.Name == bucketItemWritePermission {
			tm.bucketItemWriteGroupID = pg.ID
			return pg.ID, nil
		}
	}

	return "", fmt.Errorf("permission group %q not found", bucketItemWritePermission)
}

// doRequest makes an authenticated request to the Cloudflare API.
func (tm *TokenManager) doRequest(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfAPIBase+path, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+tm.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return tm.client.Do(req)
}

// apiError reads the response body and returns a formatted error.
func (tm *TokenManager) apiError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s: HTTP %d: %s", operation, resp.StatusCode, string(body))
}
