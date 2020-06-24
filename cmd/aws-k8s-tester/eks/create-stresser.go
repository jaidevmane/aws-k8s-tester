package eks

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-k8s-tester/eks/stresser"
	"github.com/aws/aws-k8s-tester/eksconfig"
	pkg_aws "github.com/aws/aws-k8s-tester/pkg/aws"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"github.com/aws/aws-k8s-tester/pkg/logutil"
	"github.com/aws/aws-k8s-tester/pkg/randutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	stresserKubeConfigPath string

	stresserPartition    string
	stresserRegion       string
	stresserS3BucketName string

	stresserClients       int
	stresserClientQPS     float32
	stresserClientBurst   int
	stresserClientTimeout time.Duration
	stresserObjectSize    int
	stresserListLimit     int64
	stresserDuration      time.Duration

	stresserNamespaceWrite string
	stresserNamespacesRead []string

	stresserWritesRawJSONS3Dir      string
	stresserWritesSummaryJSONS3Dir  string
	stresserWritesSummaryTableS3Dir string
	stresserReadsRawJSONS3Dir       string
	stresserReadsSummaryJSONS3Dir   string
	stresserReadsSummaryTableS3Dir  string

	stresserWritesOutputNamePrefix string
	stresserReadsOutputNamePrefix  string

	stresserBlock bool
)

func newCreateStresser() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stresser",
		Short: "Creates cluster loader",
		Run:   createStresserFunc,
	}
	cmd.PersistentFlags().StringVar(&stresserKubeConfigPath, "kubeconfig", "", "kubeconfig path (optional, should be run in-cluster, useful for local testing)")
	cmd.PersistentFlags().StringVar(&stresserPartition, "partition", "aws", "partition for AWS API")
	cmd.PersistentFlags().StringVar(&stresserRegion, "region", "us-west-2", "region for AWS API")
	cmd.PersistentFlags().StringVar(&stresserS3BucketName, "s3-bucket-name", "", "S3 bucket name to upload results")
	cmd.PersistentFlags().IntVar(&stresserClients, "clients", eksconfig.DefaultClients, "Number of clients to create")
	cmd.PersistentFlags().Float32Var(&stresserClientQPS, "client-qps", eksconfig.DefaultClientQPS, "kubelet client setup for QPS")
	cmd.PersistentFlags().IntVar(&stresserClientBurst, "client-burst", eksconfig.DefaultClientBurst, "kubelet client setup for burst")
	cmd.PersistentFlags().DurationVar(&stresserClientTimeout, "client-timeout", eksconfig.DefaultClientTimeout, "kubelet client timeout")
	cmd.PersistentFlags().IntVar(&stresserObjectSize, "object-size", 0, "Size of object per write (0 to disable writes)")
	cmd.PersistentFlags().Int64Var(&stresserListLimit, "list-limit", 0, "Maximum number of items to return for list call (0 to list all)")
	cmd.PersistentFlags().DurationVar(&stresserDuration, "duration", 5*time.Minute, "duration to run cluster loader")
	cmd.PersistentFlags().StringVar(&stresserNamespaceWrite, "namespace-write", "default", "namespaces to send writes")
	cmd.PersistentFlags().StringSliceVar(&stresserNamespacesRead, "namespaces-read", []string{"default"}, "namespaces to send reads")

	cmd.PersistentFlags().StringVar(&stresserWritesRawJSONS3Dir, "writes-raw-json-s3-dir", "", "s3 directory prefix to upload")
	cmd.PersistentFlags().StringVar(&stresserWritesSummaryJSONS3Dir, "writes-summary-json-s3-dir", "", "s3 directory prefix to upload")
	cmd.PersistentFlags().StringVar(&stresserWritesSummaryTableS3Dir, "writes-summary-table-s3-dir", "", "s3 directory prefix to upload")
	cmd.PersistentFlags().StringVar(&stresserReadsRawJSONS3Dir, "reads-raw-json-s3-dir", "", "s3 directory prefix to upload")
	cmd.PersistentFlags().StringVar(&stresserReadsSummaryJSONS3Dir, "reads-summary-json-s3-dir", "", "s3 directory prefix to upload")
	cmd.PersistentFlags().StringVar(&stresserReadsSummaryTableS3Dir, "reads-summary-table-s3-dir", "", "s3 directory prefix to upload")

	cmd.PersistentFlags().StringVar(&stresserWritesOutputNamePrefix, "writes-output-name-prefix", "", "writes results output name prefix in /var/log/")
	cmd.PersistentFlags().StringVar(&stresserReadsOutputNamePrefix, "reads-output-name-prefix", "", "reads results output name prefix in /var/log/")
	cmd.PersistentFlags().BoolVar(&stresserBlock, "block", false, "true to block process exit after cluster loader complete")
	return cmd
}

func createStresserFunc(cmd *cobra.Command, args []string) {
	// optional
	if stresserKubeConfigPath != "" && !fileutil.Exist(stresserKubeConfigPath) {
		fmt.Fprintf(os.Stderr, "kubeconfig not found %q\n", stresserKubeConfigPath)
		os.Exit(1)
	}
	if err := os.MkdirAll("/var/log", 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create dir %v\n", err)
		os.Exit(1)
	}
	if err := fileutil.IsDirWriteable("/var/log"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write dir %v\n", err)
		os.Exit(1)
	}

	lg, err := logutil.GetDefaultZapLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger %v\n", err)
		os.Exit(1)
	}

	awsCfg := &pkg_aws.Config{
		Logger:    lg,
		Partition: stresserPartition,
		Region:    stresserRegion,
	}
	awsSession, stsOutput, _, err := pkg_aws.New(awsCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create AWS session %v\n", err)
		os.Exit(1)
	}
	awsAccountID := aws.StringValue(stsOutput.Account)
	awsUserID := aws.StringValue(stsOutput.UserId)
	awsIAMRoleARN := aws.StringValue(stsOutput.Arn)
	lg.Info("created AWS session",
		zap.String("aws-account-id", awsAccountID),
		zap.String("aws-user-id", awsUserID),
		zap.String("aws-iam-role-arn", awsIAMRoleARN),
	)

	cli, err := k8s_client.NewEKS(&k8s_client.EKSConfig{
		Logger:         lg,
		KubeConfigPath: stresserKubeConfigPath,
		Clients:        stresserClients,
		ClientQPS:      stresserClientQPS,
		ClientBurst:    stresserClientBurst,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client %v\n", err)
		os.Exit(1)
	}

	stopc := make(chan struct{})

	// to randomize results output files
	// when multiple pods are created via deployment
	// we do not want each pod to write to the same file
	// we want to avoid conflicts and run checks for each pod
	// enough for make them unique per worker
	sfx := randutil.String(7)

	loader := stresser.New(stresser.Config{
		Logger:                  lg,
		Stopc:                   stopc,
		S3API:                   s3.New(awsSession),
		S3BucketName:            stresserS3BucketName,
		Client:                  cli,
		ClientTimeout:           stresserClientTimeout,
		Deadline:                time.Now().Add(stresserDuration),
		NamespaceWrite:          stresserNamespaceWrite,
		NamespacesRead:          stresserNamespacesRead,
		ObjectSize:              stresserObjectSize,
		ListLimit:               stresserListLimit,
		WritesRawJSONPath:       "/var/log/" + stresserWritesOutputNamePrefix + "-" + sfx + "-writes.json",
		WritesRawJSONS3Key:      filepath.Join(stresserWritesRawJSONS3Dir, stresserWritesOutputNamePrefix+"-"+sfx+"-writes.json"),
		WritesSummaryJSONPath:   "/var/log/" + stresserWritesOutputNamePrefix + "-" + sfx + "-writes-summary.json",
		WritesSummaryJSONS3Key:  filepath.Join(stresserWritesSummaryJSONS3Dir, stresserWritesOutputNamePrefix+"-"+sfx+"-writes-summary.json"),
		WritesSummaryTablePath:  "/var/log/" + stresserWritesOutputNamePrefix + "-" + sfx + "-writes-summary.txt",
		WritesSummaryTableS3Key: filepath.Join(stresserWritesSummaryTableS3Dir, stresserWritesOutputNamePrefix+"-"+sfx+"-writes-summary.txt"),
		ReadsRawJSONPath:        "/var/log/" + stresserReadsOutputNamePrefix + "-" + sfx + "-reads.json",
		ReadsRawJSONS3Key:       filepath.Join(stresserReadsRawJSONS3Dir, stresserReadsOutputNamePrefix+"-"+sfx+"-reads.json"),
		ReadsSummaryJSONPath:    "/var/log/" + stresserReadsOutputNamePrefix + "-" + sfx + "-reads-summary.json",
		ReadsSummaryJSONS3Key:   filepath.Join(stresserReadsSummaryJSONS3Dir, stresserReadsOutputNamePrefix+"-"+sfx+"-reads-summary.json"),
		ReadsSummaryTablePath:   "/var/log/" + stresserReadsOutputNamePrefix + "-" + sfx + "-reads-summary.txt",
		ReadsSummaryTableS3Key:  filepath.Join(stresserReadsSummaryTableS3Dir, stresserReadsOutputNamePrefix+"-"+sfx+"-reads-summary.txt"),
	})
	loader.Start()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigs:
		lg.Info("received OS signal", zap.String("signal", sig.String()))
		close(stopc)
		loader.Stop()
		os.Exit(0)
	case <-time.After(stresserDuration):
	}

	close(stopc)
	loader.Stop()

	_, _, err = loader.CollectMetrics()
	if err != nil {
		lg.Warn("failed to get metrics", zap.Error(err))
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("'aws-k8s-tester eks create stresser' success\n")

	if stresserBlock {
		lg.Info("waiting for OS signal")
		select {
		case sig := <-sigs:
			lg.Info("received OS signal", zap.String("signal", sig.String()))
			os.Exit(0)
		}
	}
}
