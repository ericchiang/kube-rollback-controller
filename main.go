package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ericchiang/k8s"
	"github.com/ericchiang/k8s/apis/extensions/v1beta1"
)

// Has a deployment gone over its progress deadline?
func deploymentFailed(d *v1beta1.Deployment) bool {
	eq := func(s *string, to string) bool {
		return s != nil && *s == to
	}
	for _, c := range d.Status.Conditions {
		// https://kubernetes.io/docs/user-guide/deployments/#failed-deployment
		if eq(c.Type, "Progressing") &&
			eq(c.Status, "False") &&
			eq(c.Reason, "ProgressDeadlineExceeded") {

			return true
		}
	}
	return false
}

// rollbackController is a controller that auto-rolls back any
// deployment that's been marked as failed.
type rollbackController struct {
	client *k8s.Client
	logger *log.Logger
}

// run causes the rollback controller to scan through all deployments,
// and roll back failed ones. It does not loop, and returns any errors
// that API calls encounter.
func (c *rollbackController) run(ctx context.Context) error {
	deployments, err := c.client.ExtensionsV1Beta1().ListDeployments(ctx, c.client.Namespace)
	if err != nil {
		return fmt.Errorf("list deployments: %v", err)
	}

	var (
		toUpdate []*v1beta1.Deployment
		failed   int
	)
	for _, d := range deployments.Items {
		if !deploymentFailed(d) {
			continue
		}

		failed++
		if d.Spec.RollbackTo == nil {
			toUpdate = append(toUpdate, d)
		}
	}

	c.logger.Printf("deployments=%d, failed=%d, rolled back=%d",
		len(deployments.Items), failed, failed-len(toUpdate))

	for _, d := range toUpdate {
		var lastRevision int64 = 0
		d.Spec.RollbackTo = &v1beta1.RollbackConfig{
			Revision: &lastRevision,
		}
		if _, err := c.client.ExtensionsV1Beta1().UpdateDeployment(ctx, d); err != nil {
			return fmt.Errorf("update deployment: %v", err)
		}
		c.logger.Printf("rolled back deployment: %s", *d.Metadata.Name)
	}
	return nil
}

// Convenience for development. Use kubectl's current context to
// fill out a client config.
func kubectlClient() (*k8s.Client, error) {
	stderr := new(bytes.Buffer)
	stdout := new(bytes.Buffer)
	cmd := exec.Command("kubectl", "config", "view", "-o", "json")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() != 0 {
			err = errors.New(strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("kubectl config failed: %v", err)
	}

	config := new(k8s.Config)
	if err := json.Unmarshal(stdout.Bytes(), config); err != nil {
		return nil, fmt.Errorf("invalid output for kubectl config view: %v", err)
	}

	return k8s.NewClient(config)
}

const (
	clientInCluster = "in-cluster"
	clientKubectl   = "kubectl"
)

func main() {
	var (
		clientType string
	)
	flag.StringVar(&clientType, "client", clientInCluster, "Strategy for initializing the Kubernetes client. Either uses 'in-cluster' or grabs current context with 'kubectl'.")
	flag.Parse()

	l := log.New(os.Stderr, "", log.LstdFlags)

	var (
		client *k8s.Client
		err    error
	)
	switch clientType {
	case clientInCluster:
		if client, err = k8s.NewInClusterClient(); err != nil {
			l.Fatalf("initialize in-cluster client: %v", err)
		}
	case clientKubectl:
		if client, err = kubectlClient(); err != nil {
			l.Fatalf("initialize client from kubectl: %v", err)
		}
	default:
		l.Fatalf("unrecognized client type: %s", clientType)
	}

	// Start the rollback controller and run forever.
	c := rollbackController{client: client, logger: l}
	for {
		if err := c.run(context.Background()); err != nil {
			l.Printf("running rollbackController: %v", err)
		}

		time.Sleep(2 * time.Second)
	}
}
