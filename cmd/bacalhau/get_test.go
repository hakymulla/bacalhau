package bacalhau

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/filecoin-project/bacalhau/pkg/computenode"
	"github.com/filecoin-project/bacalhau/pkg/ipfs"
	"github.com/filecoin-project/bacalhau/pkg/publicapi"
	"github.com/filecoin-project/bacalhau/pkg/storage/util"
	"github.com/filecoin-project/bacalhau/pkg/system"
	devstack_tests "github.com/filecoin-project/bacalhau/pkg/test/devstack"
	"github.com/phayes/freeport"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestGetSuite(t *testing.T) {
	suite.Run(t, new(GetSuite))
}

// Define the suite, and absorb the built-in basic suite
// functionality from testify - including a T() method which
// returns the current testing context
type GetSuite struct {
	suite.Suite
	rootCmd *cobra.Command
}

// Before all suite
func (suite *GetSuite) SetupAllSuite() {

}

// Before each test
func (suite *GetSuite) SetupTest() {
	require.NoError(suite.T(), system.InitConfigForTesting())
	suite.rootCmd = RootCmd
}

func (suite *GetSuite) TearDownTest() {
}

func (suite *GetSuite) TearDownAllSuite() {

}

func (suite *GetSuite) TestGetJob() {
	const NumberOfNodes = 3

	numOfJobsTests := []struct {
		numOfJobs int
	}{
		{numOfJobs: 1},
		{numOfJobs: 21}, // one more than the default list length
	}

	host := "localhost"
	port, _ := freeport.GetFreePort()
	submittedJobID := ""

	outputDir, _ := os.MkdirTemp(os.TempDir(), "bacalhau-get-test-*")
	defer os.RemoveAll(outputDir)
	for _, n := range numOfJobsTests {
		func() {
			c, cm := publicapi.SetupRequesterNodeForTestWithPort(suite.T(), port)
			defer cm.Cleanup()

			for i := 0; i < NumberOfNodes; i++ {
				for i := 0; i < n.numOfJobs; i++ {
					runNoopJob := true // Remove when gets are fixed in DevStack
					if runNoopJob {
						for i := 0; i < NumberOfNodes; i++ {
							for i := 0; i < n.numOfJobs; i++ {
								ctx := context.Background()
								j := publicapi.MakeGenericJob()
								s, err := c.Submit(ctx, j, nil)
								require.NoError(suite.T(), err)
								submittedJobID = s.ID // Default to the last job submitted, should be fine?
							}
						}
					} else {
						// Submit job and wait (so that we can download it later)
						_, out, err := ExecuteTestCobraCommand(suite.T(), suite.rootCmd, "docker", "run",
							"--api-host", host,
							"--api-port", fmt.Sprintf("%d", port),
							"ubuntu", "echo Random -> $RANDOM",
							"--wait",
						)

						require.NoError(suite.T(), err)
						submittedJobID = strings.TrimSpace(out) // Default to the last job submitted, should be fine?
					}
				}
			}

			parsedBasedURI, _ := url.Parse(c.BaseURI)
			host, port, _ := net.SplitHostPort(parsedBasedURI.Host)

			// No job id (should error)
			_, _, err := ExecuteTestCobraCommand(suite.T(), suite.rootCmd, "get",
				"--api-host", host,
				"--api-port", port,
			)
			require.Error(suite.T(), err, "Submitting a get request with no id should error.")

			outputDirWithID := filepath.Join(outputDir, submittedJobID)
			os.Mkdir(outputDirWithID, util.OS_ALL_RWX)

			// Job Id at the end
			_, _, err = ExecuteTestCobraCommand(suite.T(), suite.rootCmd, "get",
				"--api-host", host,
				"--api-port", port,
				"--output-dir", outputDirWithID,
				submittedJobID,
			)
			require.NoError(suite.T(), err, "Error in getting job: %+v", err)

			// Short Job ID
			_, _, err = ExecuteTestCobraCommand(suite.T(), suite.rootCmd, "get",
				"--api-host", host,
				"--api-port", port,
				"--output-dir", outputDirWithID,
				submittedJobID[0:8],
			)
			require.NoError(suite.T(), err, "Error in getting short job: %+v", err)

			// Get stdout from job
			// _, out, err = ExecuteTestCobraCommand(suite.T(), suite.rootCmd, "get",
			// 	"--api-host", host,
			// 	"--api-port", port,
			// 	"--output-dir", outputDirWithID,
			// 	out)

			// require.NoError(suite.T(), err, "Error in getting files from job: %+v", err)
			// // TODO: #637 Need to do a lot more testing here, we don't do any analysis of output files
			// fmt.Println(out)
		}()
	}

}

func testResultsFolderStructure(t *testing.T, baseFolder, hostID string) {
	files := []string{}
	err := filepath.Walk(baseFolder, func(path string, info os.FileInfo, err error) error {
		usePath := strings.Replace(path, baseFolder, "", 1)
		if usePath != "" {
			files = append(files, usePath)
		}
		return nil
	})
	require.NoError(t, err, "Error walking results directory")

	require.Equal(t, strings.Join([]string{
		fmt.Sprintf("/%s", ipfs.DownloadVolumesFolderName),
		fmt.Sprintf("/%s/0", ipfs.DownloadVolumesFolderName),
		fmt.Sprintf("/%s/0/node_%s_exitCode", ipfs.DownloadVolumesFolderName, system.GetShortID(hostID)),
		fmt.Sprintf("/%s/0/node_%s_stderr", ipfs.DownloadVolumesFolderName, system.GetShortID(hostID)),
		fmt.Sprintf("/%s/0/node_%s_stdout", ipfs.DownloadVolumesFolderName, system.GetShortID(hostID)),
		fmt.Sprintf("/stderr"),
		fmt.Sprintf("/stdout"),
		fmt.Sprintf("/%s", ipfs.DownloadVolumesFolderName),
		fmt.Sprintf("/%s/outputs", ipfs.DownloadVolumesFolderName),
	}, ","), strings.Join(files, ","), "The discovered results output structure was not correct")
}

func setupTempWorkingDir(t *testing.T) (string, func()) {
	// switch wd to a temp dir so we are not writing folders to the current directory
	// (the point of this test is to see what happens when we DONT pass --output-dir)
	tempDir, err := os.MkdirTemp("", "docker-run-download-test")
	require.NoError(t, err)
	originalWd, err := os.Getwd()
	require.NoError(t, err)
	err = os.Chdir(tempDir)
	require.NoError(t, err)
	return tempDir, func() {
		os.Chdir(originalWd)
	}
}

// this tests that when we do docker run with no --output-dir
// it makes it's own folder to put the results in and does not splat results
// all over the current directory
func (s *GetSuite) TestDockerRunWriteToJobFolderAutoDownload() {
	ctx := context.Background()
	stack, _ := devstack_tests.SetupTest(ctx, s.T(), 1, 0, false, computenode.ComputeNodeConfig{})
	*ODR = *NewDockerRunOptions()

	swarmAddresses, err := stack.Nodes[0].IPFSClient.SwarmAddresses(ctx)
	require.NoError(s.T(), err)

	tempDir, cleanup := setupTempWorkingDir(s.T())
	defer cleanup()

	_, out, err := ExecuteTestCobraCommand(s.T(), s.rootCmd, "docker", "run",
		"--api-host", stack.Nodes[0].APIServer.Host,
		"--api-port", fmt.Sprintf("%d", stack.Nodes[0].APIServer.Port),
		"--ipfs-swarm-addrs", strings.Join(swarmAddresses, ","),
		"--wait",
		"--download",
		"ubuntu",
		"--",
		"echo", "hello from docker submit wait",
	)
	require.NoError(s.T(), err, "Error submitting job")
	jobID := system.FindJobIDInTestOutput(out)
	hostID := stack.Nodes[0].HostID

	testResultsFolderStructure(s.T(), filepath.Join(tempDir, getDefaultJobFolder(jobID)), hostID)
}
