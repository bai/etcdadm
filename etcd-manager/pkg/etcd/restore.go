package etcd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	protoetcd "kope.io/etcd-manager/pkg/apis/etcd"
	"kope.io/etcd-manager/pkg/backup"
)

// DoRestore restores a backup from the backup store
func (s *EtcdServer) DoRestore(ctx context.Context, request *protoetcd.DoRestoreRequest) (*protoetcd.DoRestoreResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	response := &protoetcd.DoRestoreResponse{}

	if s.clusterName != request.ClusterName {
		glog.Infof("request had incorrect ClusterName.  ClusterName=%q but request=%q", s.clusterName, request)
		return nil, fmt.Errorf("ClusterName mismatch")
	}

	if !s.peerServer.IsLeader(request.LeadershipToken) {
		return nil, fmt.Errorf("LeadershipToken in request %q is not current leader", request.LeadershipToken)
	}

	if s.process == nil {
		return nil, fmt.Errorf("etcd not running")
	}

	if request.Storage == "" {
		return nil, fmt.Errorf("Storage is required")
	}
	if request.BackupName == "" {
		return nil, fmt.Errorf("BackupName is required")
	}

	backupStore, err := backup.NewStore(request.Storage)
	if err != nil {
		return nil, err
	}

	backupInfo, err := backupStore.LoadInfo(request.BackupName)
	if err != nil {
		return nil, err
	}

	isV2 := false
	if strings.HasPrefix(backupInfo.EtcdVersion, "2.") {
		isV2 = true
	}

	binDir, err := bindirForEtcdVersion(backupInfo.EtcdVersion)
	if err != nil {
		return nil, err
	}

	clusterToken := "restore-etcd-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tempDir := filepath.Join(os.TempDir(), clusterToken)
	dataDir := filepath.Join(tempDir, "data")
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		return nil, fmt.Errorf("error creating tempdir %q: %v", tempDir, err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			glog.Warningf("error cleaning up tempdir %q: %v", tempDir, err)
		}
	}()

	var downloadDir string
	if isV2 {
		downloadDir = dataDir

		if err := os.MkdirAll(dataDir, 0700); err != nil {
			return nil, fmt.Errorf("error creating datadir %q: %v", dataDir, err)
		}
	} else {
		// V3 requires that data dir not exist
		downloadDir = filepath.Join(tempDir, "download")
	}

	glog.Infof("Downloading backup %q to %s", request.BackupName, downloadDir)
	if err := backupStore.DownloadBackup(request.BackupName, downloadDir); err != nil {
		return nil, fmt.Errorf("error restoring backup: %v", err)
	}

	// TODO: randomize port
	port := 8002
	peerPort := 8003 // Needed because otherwise etcd won't start (sadly)
	clientUrl := "http://127.0.0.1:" + strconv.Itoa(port)
	peerUrl := "http://127.0.0.1:" + strconv.Itoa(peerPort)
	myNodeName := "restore"
	node := &protoetcd.EtcdNode{
		Name:       myNodeName,
		ClientUrls: []string{clientUrl},
		PeerUrls:   []string{peerUrl},
	}
	p := &etcdProcess{
		CreateNewCluster: true,
		ForceNewCluster:  true,
		BinDir:           binDir,
		EtcdVersion:      backupInfo.EtcdVersion,
		DataDir:          dataDir,
		Cluster: &protoetcd.EtcdCluster{
			ClusterToken: clusterToken,
			Nodes:        []*protoetcd.EtcdNode{node},
		},
		MyNodeName: myNodeName,
	}

	if !isV2 {
		glog.Infof("restoring snapshot")
		if err := p.RestoreV3Snapshot(downloadDir); err != nil {
			return nil, err
		}
	}

	glog.Infof("starting etcd to read backup")
	if err := p.Start(); err != nil {
		return nil, fmt.Errorf("error starting etcd: %v", err)
	}
	defer func() {
		glog.Infof("stopping etcd that was reading backup")
		err := p.Stop()
		if err != nil {
			glog.Warningf("unable to stop etcd process that was started for restore: %v", err)
		}
	}()

	if err := copyEtcd(p, s.process); err != nil {
		return nil, err
	}

	return response, nil
}

func copyEtcd(source, dest *etcdProcess) error {
	sourceClient, err := source.Client()
	if err != nil {
		return fmt.Errorf("error building etcd client: %v", err)
	}
	destClient, err := dest.Client()
	if err != nil {
		return fmt.Errorf("error building etcd client: %v", err)
	}

	for i := 0; i < 60; i++ {
		ctx := context.TODO()
		_, err := sourceClient.Get(ctx, "/", true)
		if err == nil {
			break
		}
		glog.Infof("Waiting for etcd to start (%v)", err)
		time.Sleep(time.Second)
	}

	glog.Infof("copying etcd keys from backup-restore process to new cluster")
	ctx := context.TODO()
	if err := sourceClient.CopyTo(ctx, destClient); err != nil {
		return fmt.Errorf("error dumping keys: %v", err)
	}

	return nil
}
