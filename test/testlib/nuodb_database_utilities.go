package testlib

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	v12 "k8s.io/api/core/v1"

	"github.com/Masterminds/semver"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
)

const UPGRADE_STRATEGY = `
spec:
  strategy:
    $retainKeys:
    - type
    type: Recreate
`

type ExtractedOptions struct {
	NrTePods              int
	NrSmHotCopyPods       int
	NrSmNoHotCopyPods     int
	NrSmPods              int
	DbName                string
	ClusterName           string
	EntrypointClusterName string
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

	opt.ClusterName = options.SetValues["cloud.cluster.name"]
	if len(opt.ClusterName) == 0 {
		opt.ClusterName = "cluster0"
	}

	opt.EntrypointClusterName = options.SetValues["cloud.cluster.entrypointName"]
	if len(opt.EntrypointClusterName) == 0 {
		opt.EntrypointClusterName = "cluster0"
	}

	return
}

func EnsureDatabaseNotRunning(t *testing.T, adminPod string, opt ExtractedOptions, kubectlOptions *k8s.KubectlOptions) {
	// invoke shutdown database; this may fail if the database is already NOT_RUNNING, which is okay
	k8s.RunKubectlE(t, kubectlOptions, "exec", adminPod, "--", "nuocmd", "shutdown", "database", "--db-name", opt.DbName)
	// wait for all database processes to exit
	k8s.RunKubectl(t, kubectlOptions, "exec", adminPod, "--", "nuocmd", "check", "database", "--db-name", opt.DbName, "--num-processes", "0", "--timeout", "30")
}

type DatabaseInstallationStep func(t *testing.T, options *helm.Options, helmChartReleaseName string)

func StartDatabaseTemplate(t *testing.T, namespaceName string, adminPod string, options *helm.Options, installationStep DatabaseInstallationStep, awaitDatabase bool) (helmChartReleaseName string) {
	randomSuffix := strings.ToLower(random.UniqueId())

	InjectTestValues(t, options)
	opt := GetExtractedOptions(options)

	if IsOpenShiftEnvironment(t) {
		THPReleaseName := fmt.Sprintf("thp-%s", randomSuffix)
		AddTeardown(TEARDOWN_DATABASE, func() {
			helm.Delete(t, options, THPReleaseName, true)
		})
		helm.Install(t, options, THP_HELM_CHART_PATH, THPReleaseName)

		AwaitNrReplicasReady(t, namespaceName, THPReleaseName, 1)
	}

	helmChartReleaseName = fmt.Sprintf("database-%s", randomSuffix)
	tePodNameTemplate := fmt.Sprintf("te-%s-nuodb-%s-%s", helmChartReleaseName, opt.ClusterName, opt.DbName)
	smPodName := fmt.Sprintf("sm-%s-nuodb-%s-%s", helmChartReleaseName, opt.ClusterName, opt.DbName)

	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	options.KubectlOptions = kubectlOptions

	// with Async actions which do not return a cleanup method, create the teardown(s) first
	AddTeardown(TEARDOWN_DATABASE, func() {
		helm.Delete(t, options, helmChartReleaseName, true)
		AwaitNoPods(t, namespaceName, helmChartReleaseName)
		// Delete database only when tearing down the entrypoint cluster
		if opt.ClusterName == opt.EntrypointClusterName {
			EnsureDatabaseNotRunning(t, adminPod, opt, kubectlOptions)
			DeleteDatabase(t, namespaceName, opt.DbName, adminPod)
		}
	})

	installationStep(t, options, helmChartReleaseName)

	if awaitDatabase {
		AddDiagnosticTeardown(TEARDOWN_DATABASE, t, func() {
			DescribePods(t, namespaceName, tePodNameTemplate)
			DescribePods(t, namespaceName, smPodName)
		})
		AwaitNrReplicasScheduled(t, namespaceName, tePodNameTemplate, opt.NrTePods)
		AwaitNrReplicasScheduled(t, namespaceName, smPodName, opt.NrSmPods)

		// NOTE: the Teardown logic will pick a TE/SM that is running during teardown time. Not the TE/SM that was running originally
		// this is relevant for any tests that restart TEs/SMs

		tePodName := GetPodName(t, namespaceName, tePodNameTemplate)

		AddTeardown(TEARDOWN_DATABASE, func() {
			go GetAppLog(t, namespaceName, GetPodName(t, namespaceName, tePodNameTemplate), "", &v12.PodLogOptions{Follow: true})
		})
		AwaitPodUp(t, namespaceName, tePodName, 180*time.Second)

		smPodName0 := GetPodName(t, namespaceName, smPodName)
		AddTeardown(TEARDOWN_DATABASE, func() {
			go GetAppLog(t, namespaceName, GetPodName(t, namespaceName, smPodName), "", &v12.PodLogOptions{Follow: true})
		})
		AwaitPodUp(t, namespaceName, smPodName0, 300*time.Second)

		// Await num of database processes only for single cluster deployment;
		// in multi-clusters the await logic should be called once all clusters
		// are installed with the database chart
		if opt.ClusterName == opt.EntrypointClusterName {
			AwaitDatabaseUp(t, namespaceName, adminPod, opt.DbName, opt.NrSmPods+opt.NrTePods)
		}
	}

	return
}

func InstallDatabase(t *testing.T, options *helm.Options, helmChartReleaseName string) {
	if options.Version == "" {
		helm.Install(t, options, DATABASE_HELM_CHART_PATH, helmChartReleaseName)
	} else {
		helm.Install(t, options, "nuodb/database", helmChartReleaseName)
	}
}

func StartDatabase(t *testing.T, namespace string, adminPod string, options *helm.Options) string {
	return StartDatabaseTemplate(t, namespace, adminPod, options, InstallDatabase, true)
}

func StartDatabaseNoWait(t *testing.T, namespace string, adminPod string, options *helm.Options) string {
	return StartDatabaseTemplate(t, namespace, adminPod, options, InstallDatabase, false)
}

func SetDeploymentUpgradeStrategyToRecreate(t *testing.T, namespaceName string, deploymentName string) {
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	k8s.RunKubectl(t, kubectlOptions, "patch", "deployment", deploymentName, "-p", UPGRADE_STRATEGY)
}

func RestoreDatabase(t *testing.T, namespaceName string, podName string, databaseOptions *helm.Options) {
	// run the restore chart - which flags the database to restore on next startup
	randomSuffix := strings.ToLower(random.UniqueId())

	restName := fmt.Sprintf("restore-demo-%s", randomSuffix)
	options := &helm.Options{
		SetValues: map[string]string{
			"database.name":       "demo",
			"restore.target":      "demo",
			"restore.source":      ":latest",
			"restore.autoRestart": "true",
		},
	}
	for key, value := range databaseOptions.SetValues {
		options.SetValues[key] = value
	}
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	options.KubectlOptions = kubectlOptions

	restore := func() {
		// Get restore pod logs and events on failure
		AddDiagnosticTeardown(TEARDOWN_RESTORE, t, func() {
			restorePodName := GetPodName(t, namespaceName, "restore-demo-")
			k8s.RunKubectl(t, kubectlOptions, "describe", "pod", restorePodName)
			GetAppLog(t, namespaceName, restorePodName, "restore-job", &v12.PodLogOptions{})
		})
		// Remove restore job if exist as it's not unique for a restore chart release
		k8s.RunKubectlE(t, kubectlOptions, "delete", "job", "restore-"+options.SetValues["database.name"])
		InjectTestValues(t, options)
		helm.Install(t, options, RESTORE_HELM_CHART_PATH, restName)
		AddTeardown(TEARDOWN_RESTORE, func() { helm.Delete(t, options, restName, true) })
		// Using a bit longer timeout here as we might be performing restore using
		// older image which needs to be pulled
		AwaitPodPhase(t, namespaceName, "restore-demo-", corev1.PodSucceeded, 300*time.Second)
	}

	if options.SetValues["restore.autoRestart"] == "true" {
		AwaitDatabaseRestart(t, namespaceName, podName, "demo", databaseOptions, restore)
	} else {
		restore()
	}
}

func BackupDatabase(t *testing.T, namespaceName string, podName string,
	databaseName string, backupType string, backupGroup string) string {
	opts := k8s.NewKubectlOptions("", "", namespaceName)
	output, err := k8s.RunKubectlAndGetOutputE(t, opts,
		"exec", podName, "--",
		"nuobackup", "--type", backupType, "--db-name", databaseName,
		"--group", backupGroup, "--backup-root", "/var/opt/nuodb/backup",
		"--timeout", "300",
	)
	require.NoError(t, err, "Error creating backup")
	require.True(t, strings.Contains(output, "completed"), "Error nuobackup: %s", output)
	return GetLatestBackup(t, namespaceName, podName, databaseName, backupGroup)
}

func GetLatestBackup(t *testing.T, namespaceName string, podName string,
	databaseName string, backupGroup string) string {
	opts := k8s.NewKubectlOptions("", "", namespaceName)
	backupset, err := k8s.RunKubectlAndGetOutputE(t, opts,
		"exec", podName, "--", "bash", "-c",
		"nuobackup --type report-latest --db-name "+databaseName+
			" --group "+backupGroup+" --backup-root /var/opt/nuodb/backup 2>/dev/null",
	)
	require.NoError(t, err, "Error while reporting latest backupset")
	require.True(t, backupset != "")
	return backupset
}

func CheckArchives(t *testing.T, namespaceName string, adminPod string, dbName string, numExpected int, numExpectedRemoved int) (archives []NuoDBArchive, removedArchives []NuoDBArchive) {
	options := k8s.NewKubectlOptions("", "", namespaceName)

	// check archives
	output, err := k8s.RunKubectlAndGetOutputE(t, options, "exec", adminPod, "--",
		"nuocmd", "--show-json", "get", "archives", "--db-name", dbName)
	require.NoError(t, err, output)

	err, archives = UnmarshalArchives(output)
	require.NoError(t, err)
	require.Equal(t, numExpected, len(archives), output)

	// check removed archives
	output, err = k8s.RunKubectlAndGetOutputE(t, options, "exec", adminPod, "--",
		"nuocmd", "--show-json", "get", "archives", "--db-name", dbName, "--removed")
	require.NoError(t, err, output)

	err, removedArchives = UnmarshalArchives(output)
	require.NoError(t, err)
	require.Equal(t, numExpectedRemoved, len(removedArchives), output)
	return
}

func CreateQuickstartSchema(t *testing.T, namespaceName string, adminPod string) {
	opts := k8s.NewKubectlOptions("", "", namespaceName)

	k8s.RunKubectl(t, opts,
		"exec", adminPod, "--",
		"/opt/nuodb/bin/nuosql",
		"--user", "dba",
		"--password", "secret",
		"demo",
		"--file", "/opt/nuodb/samples/quickstart/sql/create-db.sql",
	)

	// verify that the database contains the populated data
	tables, err := RunSQL(t, namespaceName, adminPod, "demo", "show schema User")
	require.NoError(t, err, "error running SQL: show schema User")
	require.True(t, strings.Contains(tables, "HOCKEY"), "tables returned: ", tables)
}

func IsRestoreRequestSupported(t *testing.T, namespaceName string, podName string) bool {
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	err := k8s.RunKubectlE(t, kubectlOptions, "exec", podName, "--",
		"bash", "-c", "nuodocker request restore -h > /dev/null")
	return err == nil
}

func CheckRestoreRequests(t *testing.T, namespaceName string, podName string, databaseName string,
	expectedValue string, expectedLegacyValue string) (string, string) {

	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	legacyRestoreRequest := ""
	restoreRequest := ""
	Await(t, func() bool {
		legacyRestoreRequest, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "exec", podName, "--",
			"nuocmd", "get", "value", "--key", fmt.Sprintf("/nuodb/nuosm/database/%s/restore", databaseName))
		// Legacy restore request is cleared async
		return err == nil && legacyRestoreRequest == expectedLegacyValue
	}, 30*time.Second)
	if IsRestoreRequestSupported(t, namespaceName, podName) {
		restoreRequest, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "exec", podName, "--",
			"nuodocker", "get", "restore-requests", "--db-name", databaseName)
		require.NoError(t, err)
		require.Equal(t, expectedValue, restoreRequest)
	}
	return restoreRequest, legacyRestoreRequest
}

func CreateNginxDeployment(t *testing.T, namespaceName string) {
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	k8s.RunKubectl(t, kubectlOptions, "create", "deployment", NGINX_DEPLOYMENT, "--image=nginx")
	k8s.RunKubectl(t, kubectlOptions, "create", "service", "clusterip", NGINX_DEPLOYMENT, "--tcp=80:80")
	podName := GetPodName(t, namespaceName, NGINX_DEPLOYMENT)
	AwaitPodUp(t, namespaceName, podName, 60*time.Second)

	AddDiagnosticTeardown(TEARDOWN_NGINX, t, func() {
		k8s.RunKubectl(t, kubectlOptions, "describe", "deployment", NGINX_DEPLOYMENT)
		podName := GetPodName(t, namespaceName, NGINX_DEPLOYMENT)
		GetAppLog(t, namespaceName, podName, "_nginx", &corev1.PodLogOptions{})
	})

	AddTeardown(TEARDOWN_NGINX, func() {
		k8s.RunKubectl(t, kubectlOptions, "delete", "service", NGINX_DEPLOYMENT)
		k8s.RunKubectl(t, kubectlOptions, "delete", "deployment", NGINX_DEPLOYMENT)
	})
}

func ServePodFileViaHTTP(t *testing.T, namespaceName string, srcPodName string, filePath string) string {
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "deployment", NGINX_DEPLOYMENT)
	if !strings.Contains(output, NGINX_DEPLOYMENT) || err != nil {
		CreateNginxDeployment(t, namespaceName)
	}

	prefix := "nginx-html"
	tmpDir, err := ioutil.TempDir("", prefix)
	require.NoError(t, err, "Unable to create TMP directory with prefix ", prefix)
	defer os.RemoveAll(tmpDir)
	fileName := filepath.Base(filePath)
	localFilePath := path.Join(tmpDir, fileName)

	k8s.RunKubectl(t, kubectlOptions, "cp", srcPodName+":"+filePath, localFilePath)
	nginxPod := GetPodName(t, namespaceName, NGINX_DEPLOYMENT)
	k8s.RunKubectl(t, kubectlOptions, "cp", localFilePath, nginxPod+":/usr/share/nginx/html/"+fileName)
	return fmt.Sprintf("http://nginx.%s.svc.cluster.local/%s", namespaceName, fileName)
}

func GetNuoDBVersion(t *testing.T, namespaceName string, options *helm.Options) string {
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)
	podName := "nuodb-version"
	InferVersionFromTemplate(t, options)
	InjectTestValues(t, options)
	defer func() {
		// Delete the pod just in case "--rm" doesn't do it
		k8s.RunKubectlE(t, kubectlOptions, "delete", "pod", podName)
	}()
	nuodbImage := fmt.Sprintf(
		"%s/%s:%s",
		options.SetValues["nuodb.image.registry"],
		options.SetValues["nuodb.image.repository"],
		options.SetValues["nuodb.image.tag"])
	output, err := k8s.RunKubectlAndGetOutputE(
		t, kubectlOptions, "run", podName,
		"--image", nuodbImage, "--restart=Never", "--rm", "--attach", "--command", "--",
		"nuodb", "--version")
	require.NoError(t, err, "Unable to check NuoDB version in helper pod")
	match := regexp.MustCompile("NuoDB Server build (.*)").FindStringSubmatch(output)
	require.NotNil(t, match, "Unable to match NuoDB version from output")
	return match[1]
}

func SkipTestOnNuoDBVersion(t *testing.T, versionCheckFunc func(*semver.Version) bool) {
	randomSuffix := strings.ToLower(random.UniqueId())
	defer Teardown(TEARDOWN_ADMIN)
	namespaceName := fmt.Sprintf("nuodbversioncheck-%s", randomSuffix)
	CreateNamespace(t, namespaceName)
	versionString := GetNuoDBVersion(t, namespaceName, &helm.Options{})
	// Select only the main NuoDB version (i.e 4.2.1) from the full version string
	versionString = regexp.MustCompile(`^([0-9]+\.[0-9]+(?:\.[0-9]+)?).*$`).
		ReplaceAllString(versionString, "${1}")
	version, err := semver.NewVersion(versionString)
	require.NoError(t, err, "Unable to parse NuoDB version", versionString)
	if versionCheckFunc(version) {
		t.Skip("Skipping test when using NuoDB version", version)
	}
}

/**
 * Skip test if NuoDB version matches the provided condition.
 *
 * For more information about supported condition strings, please
 * check https://github.com/Masterminds/semver.
 *
 */
func SkipTestOnNuoDBVersionCondition(t *testing.T, condition string) {
	SkipTestOnNuoDBVersion(t, func(version *semver.Version) bool {
		c, err := semver.NewConstraint(condition)
		require.NoError(t, err)
		return c.Check(version)
	})
}
