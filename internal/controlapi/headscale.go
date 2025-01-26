package controlapi

import (
	"context"
	"fmt"
	"strconv"
	"time"

	headscale "github.com/juanfont/headscale/gen/go/headscale/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// HeadscaleClient implements the ControlApi interface for Headscale
type HeadscaleClient struct {
	client       headscale.HeadscaleServiceClient
	headscaleUrl string
	user         string
}

// NewHeadscaleClient creates a new HeadscaleClient instance
func NewHeadscaleClient(serverAddr string, headscaleUrl string, apiKey string, user string) (*HeadscaleClient, error) {
	if serverAddr == "" || apiKey == "" || user == "" {
		return nil, fmt.Errorf("serverAddr, apiKey and user are required")
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

	return &HeadscaleClient{
		client:       headscale.NewHeadscaleServiceClient(conn),
		headscaleUrl: headscaleUrl,
		user:         user,
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
		User:      c.user,
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
		User: c.user,
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
			ID:       fmt.Sprintf("%d", m.GetId()),
			Name:     m.GetName(),
			Hostname: m.GetName(), // Headscale uses name as hostname
			Created:  created,
			LastSeen: lastSeen,
			IPAddrs:  m.GetIpAddresses(),
			IsOnline: time.Since(lastSeen) < 5*time.Minute,
			Tags:     []string{m.GetGivenName()}, // Use given name as tag
		}
	}

	return devices, nil
}

// ApproveExitNode implements ControlApi.ApproveExitNode
func (c *HeadscaleClient) ApproveExitNode(ctx context.Context, deviceID string) error {
	nodeId, err := strconv.Atoi(deviceID)
	if err != nil {
		return fmt.Errorf("failed to parse device ID: %w", err)
	}
	routesResp, err := c.client.GetNodeRoutes(ctx, &headscale.GetNodeRoutesRequest{
		NodeId: uint64(nodeId),
	})
	if err != nil {
		return fmt.Errorf("failed to get routes: %w", err)
	}

	// Enable each route
	for _, route := range routesResp.GetRoutes() {
		_, err = c.client.EnableRoute(ctx, &headscale.EnableRouteRequest{
			RouteId: route.GetId(),
		})
		if err != nil {
			return fmt.Errorf("failed to enable route %s: %w", route.GetPrefix(), err)
		}
	}

	return nil
}

// DeleteDevice implements ControlApi.DeleteDevice
func (c *HeadscaleClient) DeleteDevice(ctx context.Context, deviceID string) error {
	nodeId, err := strconv.Atoi(deviceID)
	if err != nil {
		return fmt.Errorf("failed to parse device ID: %w", err)
	}

	_, err = c.client.DeleteNode(ctx, &headscale.DeleteNodeRequest{
		NodeId: uint64(nodeId),
	})
	if err != nil {
		return fmt.Errorf("failed to delete device: %w", err)
	}

	return nil
}

// Verify interface implementation
var _ ControlApi = (*HeadscaleClient)(nil)
