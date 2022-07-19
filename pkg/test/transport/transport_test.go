package transport_test

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/filecoin-project/bacalhau/pkg/computenode"
	"github.com/filecoin-project/bacalhau/pkg/executor"
	executorNoop "github.com/filecoin-project/bacalhau/pkg/executor/noop"
	_ "github.com/filecoin-project/bacalhau/pkg/logger"
	"github.com/filecoin-project/bacalhau/pkg/requestornode"
	"github.com/filecoin-project/bacalhau/pkg/storage"
	"github.com/filecoin-project/bacalhau/pkg/system"
	"github.com/filecoin-project/bacalhau/pkg/transport/inprocess"
	"github.com/filecoin-project/bacalhau/pkg/verifier"
	verifier_noop "github.com/filecoin-project/bacalhau/pkg/verifier/noop"
	"github.com/stretchr/testify/require"

	"github.com/stretchr/testify/suite"
)

type TransportSuite struct {
	suite.Suite
}

// a normal test function and pass our suite to suite.Run
func TestTransportSuite(t *testing.T) {
	suite.Run(t, new(TransportSuite))
}

// Before all suite
func (suite *TransportSuite) SetupAllSuite() {

}

// Before each test
func (suite *TransportSuite) SetupTest() {
	system.InitConfigForTesting(suite.T())
}

func (suite *TransportSuite) TearDownTest() {
}

func (suite *TransportSuite) TearDownAllSuite() {

}

func setupTest(t *testing.T) (
	*inprocess.Transport,
	*executorNoop.Executor,
	*verifier_noop.Verifier,
	*system.CleanupManager,
) {
	cm := system.NewCleanupManager()

	noopExecutor, err := executorNoop.NewExecutor()
	require.NoError(t, err)

	noopVerifier, err := verifier_noop.NewVerifier()
	require.NoError(t, err)

	executors := map[executor.EngineType]executor.Executor{
		executor.EngineNoop: noopExecutor,
	}

	verifiers := map[verifier.VerifierType]verifier.Verifier{
		verifier.VerifierNoop: noopVerifier,
	}

	transport, err := inprocess.NewInprocessTransport()
	require.NoError(t, err)

	_, err = computenode.NewComputeNode(
		cm,
		transport,
		executors,
		verifiers,
		computenode.NewDefaultComputeNodeConfig(),
	)
	require.NoError(t, err)

	_, err = requestornode.NewRequesterNode(
		cm,
		transport,
		verifiers,
	)
	require.NoError(t, err)

	return transport, noopExecutor, noopVerifier, cm
}

func (suite *TransportSuite) TestTransportSanity() {
	cm := system.NewCleanupManager()
	defer cm.Cleanup()
	executors := map[executor.EngineType]executor.Executor{}
	verifiers := map[verifier.VerifierType]verifier.Verifier{}
	transport, err := inprocess.NewInprocessTransport()
	require.NoError(suite.T(), err)
	_, err = computenode.NewComputeNode(
		cm,
		transport,
		executors,
		verifiers,
		computenode.NewDefaultComputeNodeConfig(),
	)
	require.NoError(suite.T(), err)
	_, err = requestornode.NewRequesterNode(
		cm,
		transport,
		verifiers,
	)
	require.NoError(suite.T(), err)
}

func (suite *TransportSuite) TestSchedulerSubmitJob() {
	ctx := context.Background()
	transport, noopExecutor, _, cm := setupTest(suite.T())
	defer cm.Cleanup()

	spec := &executor.JobSpec{
		Engine:   executor.EngineNoop,
		Verifier: verifier.VerifierNoop,
		Docker: executor.JobSpecDocker{
			Image:      "image",
			Entrypoint: []string{"entrypoint"},
			Env:        []string{"env"},
		},
		Inputs: []storage.StorageSpec{
			{
				Engine: storage.IPFSDefault,
			},
		},
	}

	deal := &executor.JobDeal{
		Concurrency: 1,
	}

	jobSelected, err := transport.SubmitJob(ctx, spec, deal)
	require.NoError(suite.T(), err)

	time.Sleep(time.Second * 1)
	require.Equal(suite.T(), 1, len(noopExecutor.Jobs))
	require.Equal(suite.T(), jobSelected.ID, noopExecutor.Jobs[0].ID)
}

func (suite *TransportSuite) TestTransportEvents() {
	ctx := context.Background()
	transport, _, _, cm := setupTest(suite.T())
	defer cm.Cleanup()

	spec := &executor.JobSpec{
		Engine:   executor.EngineNoop,
		Verifier: verifier.VerifierNoop,
		Docker: executor.JobSpecDocker{
			Image:      "image",
			Entrypoint: []string{"entrypoint"},
			Env:        []string{"env"},
		},
		Inputs: []storage.StorageSpec{
			{
				Engine: storage.IPFSDefault,
			},
		},
	}

	deal := &executor.JobDeal{
		Concurrency: 1,
	}

	_, err := transport.SubmitJob(ctx, spec, deal)
	require.NoError(suite.T(), err)
	time.Sleep(time.Second * 1)

	expectedEventNames := []string{
		executor.JobEventCreated.String(),
		executor.JobEventBid.String(),
		executor.JobEventBidAccepted.String(),
		executor.JobEventResults.String(),
	}
	actualEventNames := []string{}

	for _, event := range transport.Events {
		actualEventNames = append(actualEventNames, event.EventName.String())
	}

	sort.Strings(expectedEventNames)
	sort.Strings(actualEventNames)

	require.True(suite.T(), reflect.DeepEqual(expectedEventNames, actualEventNames), "event list is correct")
}
