package controlapi

import (
	"context"
	"fmt"
	"time"

	headscale "github.com/juanfont/headscale/gen/go/headscale/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// HeadscaleClient implements the ControlApi interface for Headscale.
//
// This client stores both userId (uint64) and userName (string) because the
// Headscale gRPC API has different type requirements for different methods:
//   - CreatePreAuthKeyRequest.User is uint64
//   - ListNodesRequest.User is string
// The userName is resolved from userId during client initialization via ListUsers.
type HeadscaleClient struct {
	client       headscale.HeadscaleServiceClient
	headscaleUrl string
	userId       uint64 // Used for CreatePreAuthKey
	userName     string // Used for ListNodes
}

// NewHeadscaleClient creates a new HeadscaleClient instance
func NewHeadscaleClient(serverAddr string, headscaleUrl string, apiKey string, userId uint64) (*HeadscaleClient, error) {
	if serverAddr == "" || apiKey == "" || userId == 0 {
		return nil, fmt.Errorf("serverAddr, apiKey and userId are required")
	}

	// Create gRPC connection with API key auth
	conn, err := grpc.Dial(
		serverAddr,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
		grpc.WithPerRPCCredentials(&apiKeyAuth{key: apiKey}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to headscale: %w", err)
	}

	client := headscale.NewHeadscaleServiceClient(conn)

	// Look up the user name from the user ID
	usersResp, err := client.ListUsers(context.Background(), &headscale.ListUsersRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}

	var userName string
	for _, u := range usersResp.GetUsers() {
		if u.GetId() == userId {
			userName = u.GetName()
			break
		}
	}

	if userName == "" {
		return nil, fmt.Errorf("user with ID %d not found", userId)
	}

	return &HeadscaleClient{
		client:       client,
		headscaleUrl: headscaleUrl,
		userId:       userId,
		userName:     userName,
	}, nil
}

// apiKeyAuth implements credentials.PerRPCCredentials for API key authentication
type apiKeyAuth struct {
	key string
}

func (a *apiKeyAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + a.key,
	}, nil
}

func (a *apiKeyAuth) RequireTransportSecurity() bool {
	return true
}

// CreateKey implements ControlApi.CreateKey
func (c *HeadscaleClient) CreateKey(ctx context.Context) (*PreauthKey, error) {
	// Create an ephemeral, preauthorized key
	req := &headscale.CreatePreAuthKeyRequest{
		User:      c.userId,
		Reusable:  false,
		Ephemeral: true,
		// Expire the key in an hour - we can launch an instance and use the key in that time
		Expiration: timestamppb.New(time.Now().Add(1 * time.Hour)),
		AclTags:    []string{"tag:untrusted"},
	}

	resp, err := c.client.CreatePreAuthKey(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create preauth key: %w", err)
	}

	return &PreauthKey{
		Key:        resp.GetPreAuthKey().GetKey(),
		ControlURL: c.headscaleUrl,
		Tags:       resp.GetPreAuthKey().GetAclTags(),
	}, nil
}

// ListDevices implements ControlApi.ListDevices
func (c *HeadscaleClient) ListDevices(ctx context.Context) ([]Device, error) {
	req := &headscale.ListNodesRequest{
		User: c.userName,
	}

	resp, err := c.client.ListNodes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to list devices: %w", err)
	}

	devices := make([]Device, len(resp.Nodes))
	for i, m := range resp.Nodes {
		lastSeen := time.Now()
		if m.GetLastSeen() != nil {
			lastSeen = m.GetLastSeen().AsTime()
		}

		created := time.Now()
		if m.GetCreatedAt() != nil {
			created = m.GetCreatedAt().AsTime()
		}

		devices[i] = Device{
			headscaleID: m.GetId(),
			Name:        m.GetName(),
			Hostname:    m.GetName(), // Headscale uses name as hostname
			Created:     created,
			LastSeen:    lastSeen,
			IPAddrs:     m.GetIpAddresses(),
			IsOnline:    time.Since(lastSeen) < 5*time.Minute,
			Tags:        []string{m.GetGivenName()}, // Use given name as tag
		}
	}

	return devices, nil
}

// ApproveExitNode implements ControlApi.ApproveExitNode
func (c *HeadscaleClient) ApproveExitNode(ctx context.Context, device *Device) error {
	// Get the node to retrieve its available routes
	nodeResp, err := c.client.GetNode(ctx, &headscale.GetNodeRequest{
		NodeId: device.headscaleID,
	})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	node := nodeResp.GetNode()
	if node == nil {
		return fmt.Errorf("node not found")
	}

	// Get available routes
	availableRoutes := node.GetAvailableRoutes()
	if len(availableRoutes) == 0 {
		return fmt.Errorf("no routes available to approve")
	}

	// Approve all available routes
	_, err = c.client.SetApprovedRoutes(ctx, &headscale.SetApprovedRoutesRequest{
		NodeId: device.headscaleID,
		Routes: availableRoutes,
	})
	if err != nil {
		return fmt.Errorf("failed to approve routes: %w", err)
	}

	return nil
}

// DeleteDevice implements ControlApi.DeleteDevice
func (c *HeadscaleClient) DeleteDevice(ctx context.Context, device *Device) error {
	_, err := c.client.DeleteNode(ctx, &headscale.DeleteNodeRequest{
		NodeId: device.headscaleID,
	})
	if err != nil {
		return fmt.Errorf("failed to delete device: %w", err)
	}

	return nil
}

// Verify interface implementation
var _ ControlApi = (*HeadscaleClient)(nil)
