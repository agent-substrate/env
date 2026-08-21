package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type portForwarder struct {
	endpoint string
	stop     func()
}

func getRESTConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	configOverrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		configOverrides.CurrentContext = contextName
	}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	return clientConfig.ClientConfig()
}

func startPortForward(ctx context.Context, kubeconfigPath, contextName, namespace string, remotePort int32) (*portForwarder, error) {
	restConfig, err := getRESTConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + apiName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s pods in namespace %q: %w", apiName, namespace, err)
	}

	var targetPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			targetPod = pod
			break
		}
	}
	if targetPod == nil {
		return nil, fmt.Errorf("no running %s pod found in namespace %q", apiName, namespace)
	}

	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(targetPod.Namespace).
		Name(targetPod.Name).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating spdy roundtripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	errBuf := new(bytes.Buffer)

	pf, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", remotePort)}, stopChan, readyChan, nil, errBuf)
	if err != nil {
		return nil, fmt.Errorf("creating port forwarder: %w", err)
	}

	errChan := make(chan error, 1)
	go func() {
		if err := pf.ForwardPorts(); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-readyChan:
		ports, err := pf.GetPorts()
		if err != nil {
			close(stopChan)
			return nil, fmt.Errorf("getting forwarded ports: %w", err)
		}
		if len(ports) == 0 {
			close(stopChan)
			return nil, errors.New("no ports forwarded")
		}
		endpoint := fmt.Sprintf("127.0.0.1:%d", ports[0].Local)
		return &portForwarder{
			endpoint: endpoint,
			stop: func() {
				close(stopChan)
			},
		}, nil
	case err := <-errChan:
		errMsg := err.Error()
		if errBuf.Len() > 0 {
			errMsg += ": " + errBuf.String()
		}
		return nil, fmt.Errorf("port forward failed: %s", errMsg)
	case <-time.After(10 * time.Second):
		close(stopChan)
		return nil, fmt.Errorf("timed out waiting for port forward to pod %s", targetPod.Name)
	}
}
