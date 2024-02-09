package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/attestantio/go-eth2-client/api"
	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/base-org/blob-archiver/api/metrics"
	"github.com/base-org/blob-archiver/common/beacon/beacontest"
	"github.com/base-org/blob-archiver/common/blobtest"
	"github.com/base-org/blob-archiver/common/storage"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum-optimism/optimism/op-service/testlog"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/stretchr/testify/require"
)

func TestIsHash(t *testing.T) {
	require.True(t, isHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"))
	// Invalid hex character, ending with z
	require.False(t, isHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdez"))
	// Missing 0x prefix
	require.False(t, isHash("1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"))

	require.False(t, isHash("genesis"))
	require.False(t, isHash("finalized"))
	require.False(t, isHash("123"))     // slot
	require.False(t, isHash("unknown")) // incorrect input
}

func TestIsSlot(t *testing.T) {
	require.True(t, isSlot("123"))
	require.False(t, isSlot("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"))
	require.False(t, isSlot("genesis"))
	require.False(t, isSlot("finalized"))
	require.False(t, isSlot("unknown"))
}

func TestIsNamedIdentifier(t *testing.T) {
	require.True(t, isKnownIdentifier("genesis"))
	require.True(t, isKnownIdentifier("finalized"))
	require.False(t, isKnownIdentifier("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"))
	require.False(t, isKnownIdentifier("123"))
	require.False(t, isKnownIdentifier("unknown"))
}

func setup(t *testing.T) (*API, *storage.FileStorage, *beacontest.StubBeaconClient, func()) {
	logger := testlog.Logger(t, log.LvlInfo)
	tempDir, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	fs := storage.NewFileStorage(tempDir, logger)
	beacon := beacontest.NewEmptyStubBeaconClient()
	r := opmetrics.NewRegistry()
	m := metrics.NewMetrics(r)
	api := NewAPI(fs, beacon, m, r, logger)
	return api, fs, beacon, func() {
		require.NoError(t, os.RemoveAll(tempDir))
	}
}

func TestAPIService(t *testing.T) {
	a, fs, beaconClient, cleanup := setup(t)
	defer cleanup()

	rootOne := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	rootTwo := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890222222")

	blockOne := storage.BlobData{
		Header: storage.Header{
			BeaconBlockHash: rootOne,
		},
		BlobSidecars: storage.BlobSidecars{
			Data: blobtest.NewBlobSidecars(t, 2),
		},
	}

	blockTwo := storage.BlobData{
		Header: storage.Header{
			BeaconBlockHash: rootTwo,
		},
		BlobSidecars: storage.BlobSidecars{
			Data: blobtest.NewBlobSidecars(t, 2),
		},
	}

	err := fs.Write(context.Background(), blockOne)
	require.NoError(t, err)

	err = fs.Write(context.Background(), blockTwo)
	require.NoError(t, err)

	beaconClient.Headers["finalized"] = &v1.BeaconBlockHeader{
		Root: phase0.Root(rootOne),
	}

	beaconClient.Headers["head"] = &v1.BeaconBlockHeader{
		Root: phase0.Root(rootTwo),
	}

	beaconClient.Headers["1234"] = &v1.BeaconBlockHeader{
		Root: phase0.Root(rootTwo),
	}

	tests := []struct {
		name       string
		path       string
		status     int
		expected   *storage.BlobSidecars
		errMessage string
	}{
		{
			name:     "fetch root one",
			path:     fmt.Sprintf("/eth/v1/beacon/blob_sidecars/%s", rootOne),
			status:   200,
			expected: &blockOne.BlobSidecars,
		},
		{
			name:     "fetch root two",
			path:     fmt.Sprintf("/eth/v1/beacon/blob_sidecars/%s", rootTwo),
			status:   200,
			expected: &blockTwo.BlobSidecars,
		},
		{
			name:       "fetch unknown",
			path:       "/eth/v1/beacon/blob_sidecars/0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abc111",
			status:     404,
			errMessage: "Block not found",
		},
		{
			name:     "fetch head",
			path:     "/eth/v1/beacon/blob_sidecars/head",
			status:   200,
			expected: &blockTwo.BlobSidecars,
		},
		{
			name:     "fetch finalized",
			path:     "/eth/v1/beacon/blob_sidecars/finalized",
			status:   200,
			expected: &blockOne.BlobSidecars,
		},
		{
			name:     "fetch slot 1234",
			path:     "/eth/v1/beacon/blob_sidecars/1234",
			status:   200,
			expected: &blockTwo.BlobSidecars,
		},
		{
			name:   "indices only returns requested indices",
			path:   "/eth/v1/beacon/blob_sidecars/1234?indices=1",
			status: 200,
			expected: &storage.BlobSidecars{
				Data: []*deneb.BlobSidecar{
					blockTwo.BlobSidecars.Data[1],
				},
			},
		},
		{
			name:   "indices out of bounds returns empty array",
			path:   "/eth/v1/beacon/blob_sidecars/1234?indices=3",
			status: 200,
			expected: &storage.BlobSidecars{
				Data: []*deneb.BlobSidecar{},
			},
		},
		{
			name:       "no 0x on hash",
			path:       "/eth/v1/beacon/blob_sidecars/1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			status:     400,
			errMessage: "invalid block id: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		},
		{
			name:       "invalid hash",
			path:       "/eth/v1/beacon/blob_sidecars/0x1234567890abcdef123",
			status:     400,
			errMessage: "invalid block id: 0x1234567890abcdef123",
		},
		{
			name:       "invalid named identifier",
			path:       "/eth/v1/beacon/blob_sidecars/foobar",
			status:     400,
			errMessage: "invalid block id: foobar",
		},
		{
			name:   "invalid no parameter specified",
			path:   "/eth/v1/beacon/blob_sidecars/",
			status: 404,
		},
		{
			name:   "unknown route",
			path:   "/eth/v1/",
			status: 404,
		},
	}

	responseFormat := []string{"application/json", "application/octet-stream"}

	for _, test := range tests {
		for _, rf := range responseFormat {
			t.Run(fmt.Sprintf("%s-%s", test.name, rf), func(t *testing.T) {
				request := httptest.NewRequest("GET", test.path, nil)
				request.Header.Set("Accept", rf)

				response := httptest.NewRecorder()

				a.router.ServeHTTP(response, request)

				require.Equal(t, test.status, response.Code)

				if test.status == 200 && test.expected != nil {
					blobSidecars := storage.BlobSidecars{}

					var err error
					if rf == "application/octet-stream" {
						res := api.BlobSidecars{}
						err = res.UnmarshalSSZ(response.Body.Bytes())
						blobSidecars.Data = res.Sidecars
					} else {
						err = json.Unmarshal(response.Body.Bytes(), &blobSidecars)
					}

					require.NoError(t, err)
					require.Equal(t, *test.expected, blobSidecars)
				} else if test.status != 200 && rf == "application/json" && test.errMessage != "" {
					var e httpError
					err := json.Unmarshal(response.Body.Bytes(), &e)
					require.NoError(t, err)
					require.Equal(t, test.status, e.Code)
					require.Equal(t, test.errMessage, e.Message)
				}
			})
		}
	}
}

func TestHealthHandler(t *testing.T) {
	a, _, _, cleanup := setup(t)
	defer cleanup()

	request := httptest.NewRequest("GET", "/healthz", nil)
	response := httptest.NewRecorder()

	a.router.ServeHTTP(response, request)

	require.Equal(t, 200, response.Code)
}
