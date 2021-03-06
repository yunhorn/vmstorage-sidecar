package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmbackup/snapshot"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/actions"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/fslocal"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/fsnil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/s3remote"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/envflag"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

var (
	storageDataPath   = flag.String("storageDataPath", "victoria-metrics-data", "Path to VictoriaMetrics data. Must match -storageDataPath from VictoriaMetrics or vmstorage")
	snapshotName      = flag.String("snapshotName", "", "Name for the snapshot to backup. See https://docs.victoriametrics.com/Single-server-VictoriaMetrics.html#how-to-work-with-snapshots. There is no need in setting -snapshotName if -snapshot.createURL is set")
	snapshotCreateURL = flag.String("snapshot.createURL", "", "VictoriaMetrics create snapshot url. When this is given a snapshot will automatically be created during backup. "+
		"Example: http://victoriametrics:8428/snapshot/create . There is no need in setting -snapshotName if -snapshot.createURL is set")
	snapshotDeleteURL = flag.String("snapshot.deleteURL", "", "VictoriaMetrics delete snapshot url. Optional. Will be generated from -snapshot.createURL if not provided. "+
		"All created snapshots will be automatically deleted. Example: http://victoriametrics:8428/snapshot/delete")
	dst = flag.String("dst", "", "Where to put the backup on the remote storage. "+
		"Example: gs://bucket/path/to/backup/dir, s3://bucket/path/to/backup/dir or fs:///path/to/local/backup/dir\n"+
		"-dst can point to the previous backup. In this case incremental backup is performed, i.e. only changed data is uploaded")
	origin            = flag.String("origin", "", "Optional origin directory on the remote storage with old backup for server-side copying when performing full backup. This speeds up full backups")
	concurrency       = flag.Int("concurrency", 10, "The number of concurrent workers. Higher concurrency may reduce backup duration")
	maxBytesPerSecond = flagutil.NewBytes("maxBytesPerSecond", 0, "The maximum upload speed. There is no limit if it is set to 0")
)

var (
	access_key string
	secret_key string
	sess       *session.Session
	svc        *s3.S3
	reteion    int64 //save s3 object min
	tickMin    int   //tick of min
)

func init() {
	access_key = os.Getenv("AWS_ACCESS_KEY_ID")
	secret_key = os.Getenv("AWS_SECRET_ACCESS_KEY")
	reteionstr := os.Getenv("RETION")
	if reteionstr == "" {
		reteionstr = "30d"
	}
	reteiont, err := time.ParseDuration(reteionstr)
	if err != nil {
		panic(err)
	}
	reteion = int64(reteiont.Minutes())
	tickMinStr := os.Getenv("TICK_MIN")
	if tickMinStr == "" {
		tickMinStr = "60"
	}
	tickMin, err = strconv.Atoi(tickMinStr)
	if err != nil {
		panic(err)
	}

}

func clean(ep string) {
	if sess == nil {
		sesst, err := session.NewSession(&aws.Config{
			Credentials:      credentials.NewStaticCredentials(access_key, secret_key, ""),
			Endpoint:         aws.String(ep),
			Region:           aws.String("hongkong"),
			DisableSSL:       aws.Bool(true),
			S3ForcePathStyle: aws.Bool(true), //virtual-host style
		})
		if err != nil {
			panic(err)
		}
		sess = sesst
	}
	if svc == nil {
		svc = s3.New(sess)
	}

	buckResp, _ := svc.ListBuckets(&s3.ListBucketsInput{})
	for _, bt := range buckResp.Buckets {
		fmt.Println(*bt.Name)
		params := &s3.ListObjectsInput{
			Bucket: aws.String(*bt.Name),
		}
		resp, err := svc.ListObjects(params)
		if err != nil {
			panic(err)
		}
		now := time.Now().Add(-time.Minute * time.Duration(reteion))
		for _, item := range resp.Contents {
			if item.LastModified.Before(now) {
				log.Printf("begin.delete:%s,LastModified:%s\n", *item.Key, item.LastModified.Format("2006-01-02 15:04:05"))
				doi := &s3.DeleteObjectInput{
					Bucket: aws.String(*bt.Name),
					Key:    item.Key,
				}
				_, err := svc.DeleteObject(doi)
				if err != nil {
					// panic(err)
					log.Println("delete.s3.object.faile!", err)
					continue
				}
			}
		}
	}
	log.Println("bucket.size:", len(buckResp.Buckets))
}

func main() {

	// Write flags and help message to stdout, since it is easier to grep or pipe.
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = usage
	envflag.Parse()
	buildinfo.Init()
	logger.Init()

	if len(*snapshotCreateURL) > 0 {
		if len(*snapshotName) > 0 {
			logger.Fatalf("-snapshotName shouldn't be set if -snapshot.createURL is set, since snapshots are created automatically in this case")
		}
		logger.Infof("Snapshot create url %s", *snapshotCreateURL)
		if len(*snapshotDeleteURL) <= 0 {
			err := flag.Set("snapshot.deleteURL", strings.Replace(*snapshotCreateURL, "/create", "/delete", 1))
			if err != nil {
				logger.Fatalf("Failed to set snapshot.deleteURL flag: %v", err)
			}
		}
		logger.Infof("Snapshot delete url %s", *snapshotDeleteURL)
	}

	go beginBackup()

	c := time.Tick(time.Duration(tickMin) * time.Minute)
	go func() {
		for {
			<-c
			go beginBackup()
		}
	}()
	select {}
}

func beginBackup() {

	logger.Infof("Snapshot delete url %s", *snapshotDeleteURL)

	name, err := snapshot.Create(*snapshotCreateURL)
	if err != nil {
		logger.Fatalf("cannot create snapshot: %s", err)
	}
	err = flag.Set("snapshotName", name)
	if err != nil {
		logger.Fatalf("cannot set snapshotName flag: %v", err)
	}

	defer func() {
		err := snapshot.Delete(*snapshotDeleteURL, name)
		if err != nil {
			logger.Fatalf("cannot delete snapshot: %s", err)
		}
	}()

	dayhour := time.Now().Format("2006-01-02-15-04")
	bucket := *dst + "/" + dayhour
	srcFS, err := newSrcFS()
	if err != nil {
		logger.Fatalf("%s", err)
	}
	originFS, err := newOriginFS()
	if err != nil {
		logger.Fatalf("%s", err)
	}

	dstFS, err := newDstFS(bucket)
	if err != nil {
		logger.Fatalf("%s", err)
	}
	s := dstFS.(*s3remote.FS)
	go clean(s.CustomEndpoint)

	a := &actions.Backup{
		Concurrency: *concurrency,
		Src:         srcFS,
		Dst:         dstFS,
		Origin:      originFS,
	}
	if err := a.Run(); err != nil {
		logger.Fatalf("cannot create backup: %s", err)
	}
	srcFS.MustStop()
	dstFS.MustStop()
	originFS.MustStop()
}

func usage() {
	const s = `
vmbackup performs backups for VictoriaMetrics data from instant snapshots to gcs, s3
or local filesystem. Backed up data can be restored with vmrestore.

See the docs at https://docs.victoriametrics.com/vmbackup.html .
`
	flagutil.Usage(s)
}

func newSrcFS() (*fslocal.FS, error) {
	if len(*snapshotName) == 0 {
		return nil, fmt.Errorf("`-snapshotName` or `-snapshot.createURL` must be provided")
	}
	snapshotPath := *storageDataPath + "/snapshots/" + *snapshotName

	// Verify the snapshot exists.
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open snapshot at %q: %w", snapshotPath, err)
	}
	fi, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("cannot stat %q: %w", snapshotPath, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("snapshot %q must be a directory", snapshotPath)
	}

	fs := &fslocal.FS{
		Dir:               snapshotPath,
		MaxBytesPerSecond: maxBytesPerSecond.N,
	}
	if err := fs.Init(); err != nil {
		return nil, fmt.Errorf("cannot initialize fs: %w", err)
	}
	return fs, nil
}

func newDstFS(dst string) (common.RemoteFS, error) {
	fs, err := actions.NewRemoteFS(dst)
	if err != nil {
		return nil, fmt.Errorf("cannot parse `-dst`=%q: %w", dst, err)
	}

	return fs, nil
}

func newOriginFS() (common.OriginFS, error) {
	if len(*origin) == 0 {
		return &fsnil.FS{}, nil
	}
	fs, err := actions.NewRemoteFS(*origin)
	if err != nil {
		return nil, fmt.Errorf("cannot parse `-origin`=%q: %w", *origin, err)
	}
	return fs, nil
}
