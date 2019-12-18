package testlib

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	corev1 "k8s.io/api/core/v1"
)

type ExtractedOptions struct {
	NrTePods          int
	NrSmHotCopyPods   int
	NrSmNoHotCopyPods int
	NrSmPods          int
	DbName            string
	ClusterName		  string
}

func GetExtractedOptions(options *helm.Options) (opt ExtractedOptions) {
	var err error

	opt.NrTePods, err = strconv.Atoi(options.SetValues["database.te.replicas"])
	if err != nil {
		opt.NrTePods = 1
	}

	opt.NrSmHotCopyPods, err = strconv.Atoi(options.SetValues["database.sm.hotCopy.replicas"])
	if err != nil {
		opt.NrSmHotCopyPods = 1
	}

	opt.NrSmNoHotCopyPods, err = strconv.Atoi(options.SetValues["database.sm.noHotCopy.replicas"])
	if err != nil {
		opt.NrSmNoHotCopyPods = 0
	}

	opt.NrSmPods = opt.NrSmNoHotCopyPods + opt.NrSmHotCopyPods

	opt.DbName = options.SetValues["database.name"]
	if len(opt.DbName) == 0 {
		opt.DbName = "demo"
	}

	opt.ClusterName = options.SetValues["cloud.clusterName"]
	if len(opt.ClusterName) == 0 {
		opt.ClusterName = "cluster0"
	}

	return
}

func StartDatabase(t *testing.T, namespaceName string, adminPod string, options *helm.Options) (helmChartReleaseName string) {
	randomSuffix := strings.ToLower(random.UniqueId())

	opt := GetExtractedOptions(options)

	helmChartReleaseName = fmt.Sprintf("database-%s", randomSuffix)
	tePodNameTemplate := fmt.Sprintf("te-%s-nuodb-%s-%s", helmChartReleaseName, opt.ClusterName, opt.DbName)
	smPodName := fmt.Sprintf("sm-%s-nuodb-%s-%s", helmChartReleaseName, opt.ClusterName, opt.DbName)

	kubectlOptions := k8s.NewKubectlOptions("", "")
	options.KubectlOptions = kubectlOptions
	options.KubectlOptions.Namespace = namespaceName

	// with Async actions which do not return a cleanup method, create the teardown(s) first
	AddTeardown(TEARDOWN_DATABASE, func() {
		helm.Delete(t, options, helmChartReleaseName, true)
		AwaitNoPods(t, namespaceName, helmChartReleaseName)
		DeleteDatabase(t, namespaceName, opt.DbName, adminPod)
	})

	helm.Install(t, options, DATABASE_HELM_CHART_PATH, helmChartReleaseName)

	AwaitNrReplicasScheduled(t, namespaceName, tePodNameTemplate, opt.NrTePods)
	AwaitNrReplicasScheduled(t, namespaceName, smPodName, opt.NrSmPods)

	tePodName := GetPodName(t, namespaceName, tePodNameTemplate)
	AwaitPodStatus(t, namespaceName, tePodName, corev1.PodReady, corev1.ConditionTrue, 240*time.Second)

	smPodName0 := GetPodName(t, namespaceName, smPodName)
	AwaitPodStatus(t, namespaceName, smPodName0, corev1.PodReady, corev1.ConditionTrue, 240*time.Second)

	AwaitDatabaseUp(t, namespaceName, adminPod, opt.DbName, opt.NrSmPods+opt.NrTePods)

	return
}
