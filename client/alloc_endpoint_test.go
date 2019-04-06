package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/client/config"
	sframer "github.com/hashicorp/nomad/client/lib/streamframer"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad"
	"github.com/hashicorp/nomad/nomad/mock"
	nstructs "github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
	"github.com/ugorji/go/codec"
)

func TestAllocations_GarbageCollectAll(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	client, cleanup := TestClient(t, nil)
	defer cleanup()

	req := &nstructs.NodeSpecificRequest{}
	var resp nstructs.GenericResponse
	require.Nil(client.ClientRPC("Allocations.GarbageCollectAll", &req, &resp))
}

func TestAllocations_GarbageCollectAll_ACL(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	server, addr, root := testACLServer(t, nil)
	defer server.Shutdown()

	client, cleanup := TestClient(t, func(c *config.Config) {
		c.Servers = []string{addr}
		c.ACLEnabled = true
	})
	defer cleanup()

	// Try request without a token and expect failure
	{
		req := &nstructs.NodeSpecificRequest{}
		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollectAll", &req, &resp)
		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with an invalid token and expect failure
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1005, "invalid", mock.NodePolicy(acl.PolicyDeny))
		req := &nstructs.NodeSpecificRequest{}
		req.AuthToken = token.SecretID

		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollectAll", &req, &resp)

		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with a valid token
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1007, "valid", mock.NodePolicy(acl.PolicyWrite))
		req := &nstructs.NodeSpecificRequest{}
		req.AuthToken = token.SecretID
		var resp nstructs.GenericResponse
		require.Nil(client.ClientRPC("Allocations.GarbageCollectAll", &req, &resp))
	}

	// Try request with a management token
	{
		req := &nstructs.NodeSpecificRequest{}
		req.AuthToken = root.SecretID
		var resp nstructs.GenericResponse
		require.Nil(client.ClientRPC("Allocations.GarbageCollectAll", &req, &resp))
	}
}

func TestAllocations_GarbageCollect(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	client, cleanup := TestClient(t, func(c *config.Config) {
		c.GCDiskUsageThreshold = 100.0
	})
	defer cleanup()

	a := mock.Alloc()
	a.Job.TaskGroups[0].Tasks[0].Driver = "mock_driver"
	a.Job.TaskGroups[0].RestartPolicy = &nstructs.RestartPolicy{
		Attempts: 0,
		Mode:     nstructs.RestartPolicyModeFail,
	}
	a.Job.TaskGroups[0].Tasks[0].Config = map[string]interface{}{
		"run_for": "10ms",
	}
	require.Nil(client.addAlloc(a, ""))

	// Try with bad alloc
	req := &nstructs.AllocSpecificRequest{}
	var resp nstructs.GenericResponse
	err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp)
	require.NotNil(err)

	// Try with good alloc
	req.AllocID = a.ID
	testutil.WaitForResult(func() (bool, error) {
		// Check if has been removed first
		if ar, ok := client.allocs[a.ID]; !ok || ar.IsDestroyed() {
			return true, nil
		}

		var resp2 nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp2)
		return err == nil, err
	}, func(err error) {
		t.Fatalf("err: %v", err)
	})
}

func TestAllocations_GarbageCollect_ACL(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	server, addr, root := testACLServer(t, nil)
	defer server.Shutdown()

	client, cleanup := TestClient(t, func(c *config.Config) {
		c.Servers = []string{addr}
		c.ACLEnabled = true
	})
	defer cleanup()

	// Try request without a token and expect failure
	{
		req := &nstructs.AllocSpecificRequest{}
		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp)
		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with an invalid token and expect failure
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1005, "invalid", mock.NodePolicy(acl.PolicyDeny))
		req := &nstructs.AllocSpecificRequest{}
		req.AuthToken = token.SecretID

		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp)

		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with a valid token
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1005, "test-valid",
			mock.NamespacePolicy(nstructs.DefaultNamespace, "", []string{acl.NamespaceCapabilitySubmitJob}))
		req := &nstructs.AllocSpecificRequest{}
		req.AuthToken = token.SecretID
		req.Namespace = nstructs.DefaultNamespace

		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp)
		require.True(nstructs.IsErrUnknownAllocation(err))
	}

	// Try request with a management token
	{
		req := &nstructs.AllocSpecificRequest{}
		req.AuthToken = root.SecretID

		var resp nstructs.GenericResponse
		err := client.ClientRPC("Allocations.GarbageCollect", &req, &resp)
		require.True(nstructs.IsErrUnknownAllocation(err))
	}
}

func TestAllocations_Stats(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	client, cleanup := TestClient(t, nil)
	defer cleanup()

	a := mock.Alloc()
	require.Nil(client.addAlloc(a, ""))

	// Try with bad alloc
	req := &cstructs.AllocStatsRequest{}
	var resp cstructs.AllocStatsResponse
	err := client.ClientRPC("Allocations.Stats", &req, &resp)
	require.NotNil(err)

	// Try with good alloc
	req.AllocID = a.ID
	testutil.WaitForResult(func() (bool, error) {
		var resp2 cstructs.AllocStatsResponse
		err := client.ClientRPC("Allocations.Stats", &req, &resp2)
		if err != nil {
			return false, err
		}
		if resp2.Stats == nil {
			return false, fmt.Errorf("invalid stats object")
		}

		return true, nil
	}, func(err error) {
		t.Fatalf("err: %v", err)
	})
}

func TestAllocations_Stats_ACL(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	server, addr, root := testACLServer(t, nil)
	defer server.Shutdown()

	client, cleanup := TestClient(t, func(c *config.Config) {
		c.Servers = []string{addr}
		c.ACLEnabled = true
	})
	defer cleanup()

	// Try request without a token and expect failure
	{
		req := &cstructs.AllocStatsRequest{}
		var resp cstructs.AllocStatsResponse
		err := client.ClientRPC("Allocations.Stats", &req, &resp)
		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with an invalid token and expect failure
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1005, "invalid", mock.NodePolicy(acl.PolicyDeny))
		req := &cstructs.AllocStatsRequest{}
		req.AuthToken = token.SecretID

		var resp cstructs.AllocStatsResponse
		err := client.ClientRPC("Allocations.Stats", &req, &resp)

		require.NotNil(err)
		require.EqualError(err, nstructs.ErrPermissionDenied.Error())
	}

	// Try request with a valid token
	{
		token := mock.CreatePolicyAndToken(t, server.State(), 1005, "test-valid",
			mock.NamespacePolicy(nstructs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}))
		req := &cstructs.AllocStatsRequest{}
		req.AuthToken = token.SecretID
		req.Namespace = nstructs.DefaultNamespace

		var resp cstructs.AllocStatsResponse
		err := client.ClientRPC("Allocations.Stats", &req, &resp)
		require.True(nstructs.IsErrUnknownAllocation(err))
	}

	// Try request with a management token
	{
		req := &cstructs.AllocStatsRequest{}
		req.AuthToken = root.SecretID

		var resp cstructs.AllocStatsResponse
		err := client.ClientRPC("Allocations.Stats", &req, &resp)
		require.True(nstructs.IsErrUnknownAllocation(err))
	}
}

func TestAlloc_ExecStreaming(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s := nomad.TestServer(t, nil)
	defer s.Shutdown()
	testutil.WaitForLeader(t, s.RPC)

	c, cleanup := TestClient(t, func(c *config.Config) {
		c.Servers = []string{s.GetConfig().RPCAddr.String()}
	})
	defer cleanup()

	expectedStdout := "Hello from the other side\n"
	expectedStderr := "Hello from the other side\n"
	job := mock.BatchJob()
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Tasks[0].Config = map[string]interface{}{
		"run_for": "20s",
		"exec_command": map[string]interface{}{
			"run_for":       "1s",
			"stdout_string": expectedStdout,
			"stderr_string": expectedStderr,
			"exit_code":     3,
		},
	}

	// Wait for client to be running job
	testutil.WaitForRunning(t, s.RPC, job)

	// Get the allocation ID
	args := nstructs.AllocListRequest{}
	args.Region = "global"
	resp := nstructs.AllocListResponse{}
	require.NoError(s.RPC("Alloc.List", &args, &resp))
	require.Len(resp.Allocations, 1)
	allocID := resp.Allocations[0].ID

	// Make the request
	req := &cstructs.AllocExecRequest{
		AllocID:      allocID,
		Task:         job.TaskGroups[0].Tasks[0].Name,
		Tty:          true,
		Cmd:          []string{"placeholder command"},
		QueryOptions: nstructs.QueryOptions{Region: "global"},
	}

	// Get the handler
	handler, err := c.StreamingRpcHandler("Allocations.Exec")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	frames := make(chan *sframer.StreamFrame)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, nstructs.MsgpackHandle)

		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				t.Logf("received error decoding: %#v", err)

				errCh <- fmt.Errorf("error decoding: %v", err)
				return
			}

			var frame sframer.StreamFrame
			json.Unmarshal(msg.Payload, &frame)
			t.Logf("received message: %#v", msg)
			frames <- &frame
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, nstructs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(3 * time.Second)

	exitCode := -1
	receivedStdout := ""
	receivedStderr := ""

OUTER:
	for {
		select {
		case <-timeout:
			// time out report
			require.Equal(expectedStdout, receivedStderr, "didn't receive expected stdout")
			require.Equal(expectedStderr, receivedStderr, "didn't receive expected stderr")
			require.Equal(3, exitCode, "failed to get exit code")
			require.FailNow("timed out")
		case err := <-errCh:
			t.Fatal(err)
		case f := <-frames:
			switch {
			case f.File == "stdout" && f.FileEvent == "":
				receivedStdout += string(f.Data)
			case f.File == "stderr" && f.FileEvent == "":
				receivedStderr += string(f.Data)
			case f.FileEvent == "exit-code":
				code, err := strconv.Atoi(string(f.Data))
				require.NoError(err)
				exitCode = code
			case f.FileEvent == "close":
				// do nothing
			default:
				require.Failf("unexpected frame", "event=%#v", f)
			}

			if expectedStdout == receivedStdout && expectedStderr == receivedStderr && exitCode == 3 {
				break OUTER
			}
		}
	}
}
