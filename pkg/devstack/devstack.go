package devstack

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/filecoin-project/bacalhau/pkg/compute_node"
	"github.com/filecoin-project/bacalhau/pkg/executor"
	ipfs_cli "github.com/filecoin-project/bacalhau/pkg/ipfs/cli"
	ipfs_devstack "github.com/filecoin-project/bacalhau/pkg/ipfs/devstack"
	"github.com/filecoin-project/bacalhau/pkg/jsonrpc"
	"github.com/filecoin-project/bacalhau/pkg/requestor_node"
	"github.com/filecoin-project/bacalhau/pkg/system"
	"github.com/filecoin-project/bacalhau/pkg/transport/libp2p"
	"github.com/filecoin-project/bacalhau/pkg/types"
	"github.com/filecoin-project/bacalhau/pkg/verifier"
	"github.com/phayes/freeport"
	"github.com/rs/zerolog/log"
)

type DevStackNode struct {
	cancelContext *system.CancelContext
	ComputeNode   *compute_node.ComputeNode
	RequesterNode *requestor_node.RequesterNode
	IpfsNode      *ipfs_devstack.IPFSDevServer
	IpfsCli       *ipfs_cli.IPFSCli
	Transport     *libp2p.Libp2pTransport
	JSONRpcNode   *jsonrpc.JSONRpcServer
}

type DevStack struct {
	cancelContext *system.CancelContext
	Nodes         []*DevStackNode
}

func NewDevStack(
	cancelContext *system.CancelContext,
	count, badActors int,
	getExecutors func(ipfsMultiAddress string, nodeIndex int) (map[string]executor.Executor, error),
) (*DevStack, error) {

	nodes := []*DevStackNode{}

	for i := 0; i < count; i++ {
		log.Debug().Msgf(`Creating Node #%d`, i)

		//////////////////////////////////////
		// IPFS
		//////////////////////////////////////
		ipfsConnectAddress := ""

		if i > 0 {
			// connect the libp2p scheduler node
			firstNode := nodes[0]
			ipfsConnectAddress = firstNode.IpfsNode.SwarmAddress()
		}

		// construct the ipfs, scheduler, requester, compute and jsonRpc nodes
		ipfsNode, err := ipfs_devstack.NewDevServer(cancelContext, true)
		if err != nil {
			return nil, err
		}

		err = ipfsNode.Start(ipfsConnectAddress)
		if err != nil {
			return nil, err
		}

		log.Debug().Msgf("IPFS dev server started: %s", ipfsNode.ApiAddress())

		//////////////////////////////////////
		// Scheduler
		//////////////////////////////////////
		libp2pPort, err := freeport.GetFreePort()
		if err != nil {
			return nil, err
		}

		transport, err := libp2p.NewLibp2pTransport(cancelContext, libp2pPort)
		if err != nil {
			return nil, err
		}

		//////////////////////////////////////
		// Requestor node
		//////////////////////////////////////
		requesterNode, err := requestor_node.NewRequesterNode(transport)
		if err != nil {
			return nil, err
		}

		//////////////////////////////////////
		// Compute node
		//////////////////////////////////////
		executors, err := getExecutors(ipfsNode.ApiAddress(), i)
		if err != nil {
			return nil, err
		}

		verifiers, err := verifier.NewIPFSVerifiers(cancelContext, ipfsNode.ApiAddress())
		if err != nil {
			return nil, err
		}

		computeNode, err := compute_node.NewComputeNode(transport, executors, verifiers)
		if err != nil {
			return nil, err
		}

		//////////////////////////////////////
		// JSON RPC
		//////////////////////////////////////
		jsonRpcPort, err := freeport.GetFreePort()
		if err != nil {
			return nil, err
		}

		jsonRpcNode := jsonrpc.NewBacalhauJsonRpcServer(
			cancelContext,
			"0.0.0.0",
			jsonRpcPort,
			requesterNode,
		)
		if err != nil {
			return nil, err
		}

		err = jsonrpc.StartBacalhauJsonRpcServer(jsonRpcNode)
		if err != nil {
			return nil, err
		}

		log.Debug().Msgf("JSON RPC server started: %d", jsonRpcPort)

		//////////////////////////////////////
		// intra-connections
		//////////////////////////////////////
		err = transport.Start()
		if err != nil {
			return nil, err
		}

		log.Debug().Msgf("libp2p server started: %d", libp2pPort)

		if len(nodes) > 0 {
			// connect the libp2p scheduler node
			firstNode := nodes[0]

			// get the libp2p id of the first scheduler node
			libp2pHostId, err := firstNode.Transport.HostId()
			if err != nil {
				return nil, err
			}

			// connect this scheduler to the first
			firstSchedulerAddress := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", firstNode.Transport.Port, libp2pHostId)
			log.Debug().Msgf("Connect to first libp2p scheduler node: %s", firstSchedulerAddress)
			err = transport.Connect(firstSchedulerAddress)
			if err != nil {
				return nil, err
			}
		}

		devStackNode := &DevStackNode{
			cancelContext: cancelContext,
			ComputeNode:   computeNode,
			RequesterNode: requesterNode,
			IpfsNode:      ipfsNode,
			IpfsCli:       ipfs_cli.NewIPFSCli(ipfsNode.Repo),
			Transport:     transport,
			JSONRpcNode:   jsonRpcNode,
		}

		nodes = append(nodes, devStackNode)
	}

	stack := &DevStack{
		cancelContext: cancelContext,
		Nodes:         nodes,
	}

	return stack, nil
}

func (stack *DevStack) PrintNodeInfo() {

	logString := `
-------------------------------
ipfs
-------------------------------

command="add -q testdata/grep_file.txt"
	`
	for _, node := range stack.Nodes {

		logString = logString + fmt.Sprintf(`
cid=$(IPFS_PATH=%s ipfs $command)`, node.IpfsNode.Repo)

	}

	logString += fmt.Sprintf(`

-------------------------------
jsonrpc
-------------------------------

go run . --jsonrpc-port=%d list
go run . --jsonrpc-port=%d run -v $cid:/file.txt -c 3 ubuntu cat /file.txt
`, stack.Nodes[0].JSONRpcNode.Port, stack.Nodes[0].JSONRpcNode.Port)

	for _, node := range stack.Nodes {

		logString = logString + fmt.Sprintf(`
export PORT=%d`, node.JSONRpcNode.Port)
	}

	log.Info().Msg(logString)
}

func (stack *DevStack) AddFileToNodes(nodeCount int, filePath string) (string, error) {

	returnFileCid := ""

	// ipfs add the file to 2 nodes
	// this tests self selection
	for i, node := range stack.Nodes {
		if i >= nodeCount {
			continue
		}

		nodeId, err := node.ComputeNode.Transport.HostId()

		if err != nil {
			return "", err
		}

		fileCid, err := node.IpfsCli.Run([]string{
			"add", "-Q", filePath,
		})

		if err != nil {
			return "", err
		}

		fileCid = strings.TrimSpace(fileCid)
		returnFileCid = fileCid
		log.Debug().Msgf("Added CID: %s to NODE: %s", fileCid, nodeId)
	}

	return returnFileCid, nil
}

func (stack *DevStack) AddTextToNodes(nodeCount int, fileContent []byte) (string, error) {
	testDir, err := ioutil.TempDir("", "bacalhau-test")

	if err != nil {
		return "", err
	}

	testFilePath := fmt.Sprintf("%s/test.txt", testDir)
	err = os.WriteFile(testFilePath, fileContent, 0644)
	if err != nil {
		return "", err
	}

	return stack.AddFileToNodes(nodeCount, testFilePath)
}

func (stack *DevStack) GetJobStates(jobId string) ([]string, error) {
	result, err := jsonrpc.ListJobs("127.0.0.1", stack.Nodes[0].JSONRpcNode.Port)
	if err != nil {
		return []string{}, err
	}

	var jobData *types.Job

	for _, j := range result.Jobs {
		if j.Id == jobId {
			jobData = j
			break
		}
	}

	if jobData == nil {
		return []string{}, fmt.Errorf("job not found")
	}

	jobStates := []string{}

	for _, state := range jobData.State {
		jobStates = append(jobStates, state.State)
	}

	return jobStates, nil
}

func (stack *DevStack) WaitForJob(
	jobId string,
	// a map of job states onto the number of those states we expect to see
	expectedStates map[string]int,
	// a list of states that if any job gets into is an immediate error
	errorStates []string,
) error {

	waiter := &system.FunctionWaiter{
		Name:        "wait for job",
		MaxAttempts: 100,
		Delay:       time.Second * 1,
		Logging:     true,
		Handler: func() (bool, error) {

			// load the current states of the job
			states, err := stack.GetJobStates(jobId)
			if err != nil {
				return false, err
			}

			// collect a count of the states we saw
			foundStates := map[string]int{}
			for _, state := range states {
				for _, errorState := range errorStates {
					if state == errorState {
						return true, fmt.Errorf("job has error state: %s", state)
					}
				}
				if _, ok := foundStates[state]; !ok {
					foundStates[state] = 0
				}
				foundStates[state] = foundStates[state] + 1
			}

			// now compare the found states to the expected states
			for expectedState, expectedCount := range expectedStates {
				foundCount := 0
				if _, ok := foundStates[expectedState]; ok {
					foundCount = foundStates[expectedState]
				}
				if foundCount != expectedCount {
					return false, fmt.Errorf("job has %d %s states, expected %d", foundCount, expectedState, expectedCount)
				}
			}

			// if we got to here - then the expected states line up with the actual ones
			return true, nil
		},
	}

	return waiter.Wait()
}

func (stack *DevStack) WaitForJobWithError(
	jobId string,
	expectedStates map[string]int,
) error {
	return stack.WaitForJob(jobId, expectedStates, []string{system.JOB_STATE_ERROR})
}

func (stack *DevStack) WaitForJobWithConcurrency(
	jobId string,
	concurrency int,
) error {
	expectedStates := map[string]int{}
	expectedStates[system.JOB_STATE_COMPLETE] = concurrency
	return stack.WaitForJobWithError(jobId, expectedStates)
}
