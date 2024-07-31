package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultDelay = 2 * time.Minute
)

var (
	restartCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "deployment_rollouts_total",
			Help: "Total number of deployment rollouts",
		},
		[]string{"namespace", "secret", "deployment", "restarted"},
	)
)

func init() {
	prometheus.MustRegister(restartCounter)
}

func main() {
	secretName := flag.String("secret-name", "", "Name of the secret to watch")
	deploymentName := flag.String("deployment-name", "", "Name of the deployment to restart")
	namespace := flag.String("namespace", "default", "Namespace of the secret and deployment")
	insideCluster := flag.Bool("inside-cluster", false, "Run from inside the cluster")
	delay := flag.Duration("delay", defaultDelay, "Delay before restarting the deployment")

	flag.Parse()

	if *secretName == "" || *deploymentName == "" {
		fmt.Println("secret-name and deployment-name are required")
		flag.Usage()
		os.Exit(1)
	}

	var config *rest.Config
	var err error

	if *insideCluster {
		config, err = rest.InClusterConfig()
	} else {
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Minute*10, informers.WithNamespace(*namespace))
	secretInformer := factory.Core().V1().Secrets().Informer()

	stopCh := make(chan struct{})
	defer close(stopCh)

	go secretInformer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, secretInformer.HasSynced) {
		panic("Failed to sync cache")
	}

	secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			fmt.Printf("Secret %s changed, waiting for %s before restarting deployment %s\n", *secretName, delay, *deploymentName)
			restartDeployment(clientset, *namespace, *secretName, *deploymentName, *delay)
		},
	})

	// Start Prometheus metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(":8080", nil)
	}()

	fmt.Printf("Watching secret %s in namespace %s\n", *secretName, *namespace)
	<-stopCh
}

func restartDeployment(clientset *kubernetes.Clientset, namespace, secretName, deploymentName string, delay time.Duration) {
	time.Sleep(delay)

	deploymentsClient := clientset.AppsV1().Deployments(namespace)
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of the deployment
		deployment, getErr := deploymentsClient.Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if getErr != nil {
			fmt.Printf("Failed to get latest version of Deployment: %v\n", getErr)
			restartCounter.WithLabelValues(namespace, secretName, deploymentName, "false").Inc()
			return getErr
		}

		// Increment the annotation to force the deployment to rollout
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

		_, updateErr := deploymentsClient.Update(context.TODO(), deployment, metav1.UpdateOptions{})
		return updateErr
	})
	if retryErr != nil {
		fmt.Printf("Failed to update Deployment: %v\n", retryErr)
		restartCounter.WithLabelValues(namespace, secretName, deploymentName, "false").Inc()
	} else {
		fmt.Printf("Deployment %s restarted successfully\n", deploymentName)
		restartCounter.WithLabelValues(namespace, secretName, deploymentName, "true").Inc()
	}
}
