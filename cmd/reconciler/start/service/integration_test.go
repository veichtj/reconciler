package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kyma-incubator/reconciler/pkg/model"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	cliRecon "github.com/kyma-incubator/reconciler/internal/cli/reconciler"
	cliTest "github.com/kyma-incubator/reconciler/internal/cli/test"
	"github.com/kyma-incubator/reconciler/pkg/logger"
	"github.com/kyma-incubator/reconciler/pkg/reconciler"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes/adapter"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes/progress"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/service"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/workspace"
	"github.com/kyma-incubator/reconciler/pkg/test"
	"github.com/stretchr/testify/require"
	clientgo "k8s.io/client-go/kubernetes"
)

const (
	urlCallbackHTTPBin = "https://httpbin.org/post"
	urlCallbackMock    = "http://localhost:11111/callback"
	mockServerPort     = 11111

	urlReconcilerRun = "http://localhost:9999/v1/run"

	componentReconcilerName = "istio-configuration"
	workerTimeout           = 1 * time.Minute
	serverPort              = 9999

	componentName      = "istio-configuration"
	componentNamespace = "istio-system"
	componentVersion   = "main"
	componentPod       = "dummy-pod"
)

type testCase struct {
	name               string
	model              *reconciler.Task
	expectedHTTPCode   int
	expectedResponse   interface{}
	verifyCallbacksFct func(t *testing.T, callbacks []*reconciler.CallbackMessage)
	verifyResponseFct  func(*testing.T, interface{})
}

func TestReconciler(t *testing.T) {
	//test.IntegrationTest(t)
	//ToDo Adjust path here
	os.Setenv("ISTIOCTL_PATH", "/Users/I551617/Code/BTP/istio-installation/istio-1.11.2/bin/istioctl")
	setGlobalWorkspaceFactory(t)
	//ToDo check KUBECONFIG set
	kubeClient := newKubeClient(t)
	recon := newComponentReconciler(t) //register and configure a new component reconciler

	//cleanup old pods before test execution in case previous execution failed before/during the delete test
	service.NewTestCleanup(recon, kubeClient).RemoveKymaComponent(t, componentVersion, componentName, componentNamespace)

	//create runtime context which is cancelled at the end of the test
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(1 * time.Second) //give component reconciler some time for graceful shutdown
	}()
	startReconciler(ctx, t)

	//	runTestCases(t, kubeClient)
}

func setGlobalWorkspaceFactory(t *testing.T) {
	//use ./test folder as workspace
	wsf, err := workspace.NewFactory(nil, "test", logger.NewLogger(true))
	require.NoError(t, err)
	require.NoError(t, service.UseGlobalWorkspaceFactory(wsf))
}

func newKubeClient(t *testing.T) kubernetes.Client {
	//create kubeClient (e.g. needed to verify reconciliation results)
	kubeClient, err := adapter.NewKubernetesClient(test.ReadKubeconfig(t), logger.NewLogger(true), nil)
	require.NoError(t, err)

	return kubeClient
}

func newComponentReconciler(t *testing.T) *service.ComponentReconciler {
	//create reconciler
	istio.NewIstioReconcilerComponent()
	recon, err := service.GetReconciler(componentReconcilerName)

	require.NoError(t, err)
	//configure reconciler
	return recon.Debug().WithDependencies("abc", "xyz")
}

func startReconciler(ctx context.Context, t *testing.T) {
	go func() {
		o := cliRecon.NewOptions(cliTest.NewTestOptions(t))
		o.ServerConfig.Port = serverPort
		o.WorkerConfig.Timeout = workerTimeout
		o.HeartbeatSenderConfig.Interval = 1 * time.Second
		o.ProgressTrackerConfig.Interval = 1 * time.Second
		o.RetryConfig.RetryDelay = 2 * time.Second
		o.RetryConfig.MaxRetries = 3

		workerPool, err := StartComponentReconciler(ctx, o, componentReconcilerName)
		require.NoError(t, err)

		require.NoError(t, StartWebserver(ctx, o, workerPool))
	}()
	cliTest.WaitForTCPSocket(t, "localhost", serverPort, 5*time.Second)
}

func post(t *testing.T, testCase testCase) interface{} {
	jsonPayload, err := json.Marshal(testCase.model)
	require.NoError(t, err)

	//nolint:gosec //in test cases is a dynamic URL acceptable
	t.Logf("Sending post request to component reconciler (%s)", urlReconcilerRun)
	resp, err := http.Post(urlReconcilerRun, "application/json",
		bytes.NewBuffer(jsonPayload))
	require.NoError(t, err)
	require.Equal(t, testCase.expectedHTTPCode, resp.StatusCode)

	//read body
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()
	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)

	t.Logf("Body received from component reconciler: %s", string(body))

	require.NoError(t, json.Unmarshal(body, testCase.expectedResponse))
	return testCase.expectedResponse
}

func runTestCases(t *testing.T, kubeClient kubernetes.Client) {
	//execute test cases
	testCases := []testCase{

		{
			name: "Delete component",
			model: &reconciler.Task{
				ComponentsReady: []string{"abc", "xyz"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         componentVersion,
				Type:            model.OperationTypeDelete,
				Profile:         "",
				Configuration:   nil,
				Kubeconfig:      test.ReadKubeconfig(t),
				CorrelationID:   "test-correlation-id",
			},
			expectedHTTPCode: http.StatusOK,
			expectedResponse: &reconciler.HTTPReconciliationResponse{},
			verifyResponseFct: func(t *testing.T, i interface{}) {
				expectPodInState(t, progress.TerminatedState, kubeClient) // check that deletion was successful
			},
			verifyCallbacksFct: expectSuccessfulReconciliation,
		},

		/* ToDo if you want to install from scratch de-comment this test
		{
			name: "Install component from scratch",
			model: &reconciler.Task{
				ComponentsReady: []string{"abc", "xyz"},
				Component:       componentName,
				Namespace:       componentNamespace,
				Version:         componentVersion,
				Type:            model.OperationTypeReconcile,
				Profile:         "",
				Configuration:   nil,
				Kubeconfig:      test.ReadKubeconfig(t),
				CorrelationID:   "test-correlation-id",
			},
			expectedHTTPCode: http.StatusOK,
			expectedResponse: &reconciler.HTTPReconciliationResponse{},
			verifyResponseFct: func(t *testing.T, i interface{}) {
				expectPodInState(t, progress.ReadyState, kubeClient) //wait until pod is ready
			},
			verifyCallbacksFct: expectSuccessfulReconciliation,
		}, */
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, newTestFct(testCase))
	}
}

func expectSuccessfulReconciliation(t *testing.T, callbacks []*reconciler.CallbackMessage) {
	for idx, callback := range callbacks { //callbacks are sorted in the sequence how they were retrieved
		if idx < len(callbacks)-1 {
			require.Equal(t, reconciler.StatusRunning, callback.Status)
		} else {
			require.Equal(t, reconciler.StatusSuccess, callback.Status)
		}
	}
}

func expectFailingReconciliation(t *testing.T, callbacks []*reconciler.CallbackMessage) {
	for idx, callback := range callbacks { //callbacks are sorted in the sequence how they were retrieved
		switch idx {
		case 0:
			//first callback has to indicate a running reconciliation
			require.Equal(t, reconciler.StatusRunning, callback.Status)
		case len(callbacks) - 1:
			//last callback has to indicate an error
			require.Equal(t, reconciler.StatusError, callback.Status)
		default:
			//callbacks during the reconciliation is ongoing have to indicate a failure or running
			require.Contains(t, []reconciler.Status{
				reconciler.StatusFailed,
				reconciler.StatusRunning,
			}, callback.Status)
		}
	}
}

func newProgressTracker(t *testing.T, clientSet clientgo.Interface) *progress.Tracker {
	prog, err := progress.NewProgressTracker(clientSet, logger.NewLogger(true), progress.Config{
		Interval: 1 * time.Second,
	})
	require.NoError(t, err)
	return prog
}

//newTestFct is required to make the linter happy ;)
func newTestFct(testCase testCase) func(t *testing.T) {
	return func(t *testing.T) {
		var callbackC chan *reconciler.CallbackMessage

		if testCase.model.CallbackURL == "" { //set fallback callback URL
			if testCase.verifyCallbacksFct == nil { //check if validation of callback events has to happen
				testCase.model.CallbackURL = urlCallbackHTTPBin
			} else {
				testCase.model.CallbackURL = urlCallbackMock

				//start mock server to catch callback events
				var server *http.Server
				server, callbackC = newCallbackMock(t)
				defer func() {
					require.NoError(t, server.Shutdown(context.Background()))
					time.Sleep(1 * time.Second) //give the server some time for graceful shutdown
				}()
			}
		}

		respModel := post(t, testCase)
		if testCase.verifyResponseFct != nil {
			testCase.verifyResponseFct(t, respModel)
		}

		if testCase.verifyCallbacksFct != nil {
			received := receiveCallbacks(t, callbackC)
			testCase.verifyCallbacksFct(t, received)
		}
	}
}

func newCallbackMock(t *testing.T) (*http.Server, chan *reconciler.CallbackMessage) {
	callbackC := make(chan *reconciler.CallbackMessage, 100) //don't block

	router := mux.NewRouter()
	router.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		defer require.NoError(t, r.Body.Close())
		require.NoError(t, err, "Failed to read HTTP callback message")

		t.Logf("Callback mock server received following callback request: %s", string(body))

		callbackData := &reconciler.CallbackMessage{}
		require.NoError(t, json.Unmarshal(body, &callbackData))

		callbackC <- callbackData
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", mockServerPort),
		Handler: router,
	}
	go func() {
		t.Logf("Starting callback mock server on port %d", mockServerPort)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			t.Logf("Failed to start callback mock server: %s", err)
		}
		t.Log("Shutting down callback mock server")
	}()
	cliTest.WaitForTCPSocket(t, "localhost", mockServerPort, 3*time.Second)

	return srv, callbackC
}

func receiveCallbacks(t *testing.T, callbackC chan *reconciler.CallbackMessage) []*reconciler.CallbackMessage {
	var received []*reconciler.CallbackMessage
Loop:
	for {
		select {
		case callback := <-callbackC:
			received = append(received, callback)
			if callback.Status == reconciler.StatusError || callback.Status == reconciler.StatusSuccess {
				break Loop
			}
		case <-time.NewTimer(workerTimeout).C:
			t.Logf("Timeout reached for retrieving callbacks")
			break Loop
		}
	}
	return received
}

func expectPodInState(t *testing.T, state progress.State, kubeClient kubernetes.Client) {
	clientSet, err := kubeClient.Clientset()
	require.NoError(t, err)

	watchable, err := progress.NewWatchableResource("pod")
	require.NoError(t, err)

	t.Logf("Waiting for pod '%s' to reach %s state", componentPod, strings.ToUpper(string(state)))
	prog := newProgressTracker(t, clientSet)
	prog.AddResource(watchable, componentNamespace, componentPod)
	require.NoError(t, prog.Watch(context.TODO(), state))
	t.Logf("Pod '%s' reached %s state", componentPod, strings.ToUpper(string(state)))
}
