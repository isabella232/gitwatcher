//go:generate go run types/codegen/cleanup/main.go
//go:generate go run types/codegen/main.go

package main

import (
	"context"
	"net/http"
	"os"

	"github.com/rancher/webhookinator/pkg/hooks"
	"github.com/rancher/webhookinator/types"
	"github.com/rancher/wrangler/pkg/leader"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	VERSION = "v0.0.0-dev"
)

func main() {
	app := cli.NewApp()
	app.Name = "webhookinator"
	app.Version = VERSION
	app.Usage = "webhookinator needs help!"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "kubeconfig",
			EnvVar: "KUBECONFIG",
			Value:  "${HOME}/.kube/config",
		},
		cli.StringFlag{
			Name:  "listen-address",
			Value: ":8888",
		},
	}
	app.Action = run

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func run(c *cli.Context) error {
	logrus.Info("Starting controller")

	ctx := signals.SetupSignalHandler(context.Background())
	kubeconfig := c.String("kubeconfig")
	namespace := os.Getenv("NAMESPACE")

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}
	ctx, rioContext := types.BuildContext(ctx, namespace, restConfig)

	leader.RunOrDie(ctx, namespace, "rio", rioContext.K8s, func(ctx context.Context) {
		runtime.Must(rioContext.Start(ctx))
		<-ctx.Done()
	})

	addr := c.String("listen-address")
	logrus.Infof("Listening on %s", addr)
	handler := hooks.HandleHooks(rioContext)
	go func() {
		if err := http.ListenAndServe(addr, handler); err != nil {
			logrus.Fatalf("Failed to listen on %s: %v", addr, err)
			return
		}
	}()
	<-ctx.Done()
	return nil
}
