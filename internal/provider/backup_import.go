// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	// sourceInstanceLabel and sourceNamespaceLabel record, on an imported
	// Backup, the Instance and namespace the backup was taken from, as read
	// from its path in the storage. They let a new Instance be seeded from
	// the point-in-time stream of an Instance that no longer exists.
	sourceInstanceLabel  = "backup.provider-mariadb.openeverest.io/source-instance"
	sourceNamespaceLabel = "backup.provider-mariadb.openeverest.io/source-namespace"

	// sizeAnnotation records the size in bytes of an imported backup file;
	// the Backup API has no field for it.
	sizeAnnotation = "backup.provider-mariadb.openeverest.io/size-bytes"

	// binlogIndexFile is the index the operator keeps of the binary logs it
	// archives, under the binlog directory of a storage path.
	binlogIndexFile = "index.yaml"
	// maxBinlogIndexSize caps how much of a binlog index is read. An entry is
	// a few hundred bytes, so this holds tens of thousands of binary logs.
	maxBinlogIndexSize = 16 << 20

	// The operator's file name prefixes and timestamp layout of physical
	// backups (pkg/backup and pkg/time in mariadb-operator).
	logicalBackupPrefix  = "backup."
	physicalBackupPrefix = "physicalbackup-"
	physicalBackupLayout = "20060102150405"

	// backupsPathSegment is the first segment of the nested layout the
	// runtime writes backups to: backups/<namespace>/<instance>/<uid>/<storage>.
	backupsPathSegment = "backups"
)

// importS3Client is the part of the S3 API the importer uses.
type importS3Client interface {
	s3.ListObjectsV2APIClient
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// newImportS3Client builds the S3 client of a BackupStorage. Tests replace it.
var newImportS3Client = func(
	ctx context.Context, c client.Client, storage *backupv1alpha1.BackupStorage,
) (importS3Client, error) {
	return controller.NewS3Client(ctx, c, storage)
}

// storedObject is an object found under the storage's prefix, keyed relative
// to it.
type storedObject struct {
	key          string
	size         int64
	lastModified time.Time
}

// binlogIndex is the part of the operator's binlog index (pkg/binlog in
// mariadb-operator) the importer reads.
type binlogIndex struct {
	Binlogs map[string][]binlogMetadata `json:"binlogs"`
}

type binlogMetadata struct {
	ServerVersion string      `json:"serverVersion"`
	LastTime      metav1.Time `json:"lastTime"`
}

// serverVersion returns the server version of the most recently written
// binary log in the index, or "" when it records none.
func (b *binlogIndex) serverVersion() string {
	var version string
	var last time.Time
	for _, binlogs := range b.Binlogs {
		for _, m := range binlogs {
			if m.ServerVersion != "" && !m.LastTime.Time.Before(last) {
				version, last = m.ServerVersion, m.LastTime.Time
			}
		}
	}
	return version
}

// ImportBackups implements controller.BackupImporter. It lists the objects
// under the storage's prefix and returns an external Backup for every logical
// (mariadb-dump) and physical (mariadb-backup) backup file the operator wrote
// there. Archived binary logs are not backups of their own: they are replayed
// on top of a physical backup of the same storage path when seeding a new
// Instance from a point in time, and their index supplies the engine version.
func (p *MariaDBProvider) ImportBackups(
	ctx context.Context,
	imp *backupv1alpha1.BackupImport,
	storage *backupv1alpha1.BackupStorage,
) (controller.BackupImportExecutionStatus, error) {
	l := log.FromContext(ctx).WithValues("backupImport", imp.Name, "storage", storage.Name)

	if storage.Spec.Location.ObjectStore == nil {
		return controller.BackupImportExecutionStatus{
			State:   backupv1alpha1.BackupImportStateFailed,
			Message: fmt.Sprintf("BackupStorage %q has no object store location", storage.Name),
		}, nil
	}
	if p.client == nil {
		return controller.BackupImportExecutionStatus{}, errors.New("provider client not set")
	}
	s3c, err := newImportS3Client(ctx, p.client, storage)
	if err != nil {
		return controller.BackupImportExecutionStatus{}, fmt.Errorf("build S3 client: %w", err)
	}

	bucket := storage.Spec.Location.ObjectStore.Bucket
	prefix := backupv1alpha1.BackupKeyPrefix(backupv1alpha1.BackupPathNested, storage.Spec.Location)
	objects, err := listStoredObjects(ctx, s3c, bucket, prefix)
	if err != nil {
		return controller.BackupImportExecutionStatus{}, err
	}

	versions := map[string]string{}
	for _, obj := range objects {
		dir, ok := binlogIndexDir(obj.key)
		if !ok {
			continue
		}
		data, err := getStoredObject(ctx, s3c, bucket, prefix+obj.key, maxBinlogIndexSize)
		if err != nil {
			return controller.BackupImportExecutionStatus{}, fmt.Errorf("read %q: %w", prefix+obj.key, err)
		}
		index := &binlogIndex{}
		if err := yaml.Unmarshal(data, index); err != nil {
			l.Error(err, "skipping unparseable binlog index", "key", prefix+obj.key)
			continue
		}
		versions[dir] = index.serverVersion()
	}

	backups := importedBackups(imp, objects, versions)
	l.Info("discovered MariaDB backups", "count", len(backups))
	return controller.BackupImportExecutionStatus{
		Backups: backups,
		State:   backupv1alpha1.BackupImportStateSucceeded,
	}, nil
}

// listStoredObjects lists every object under prefix, keyed relative to it.
func listStoredObjects(ctx context.Context, s3c importS3Client, bucket, prefix string) ([]storedObject, error) {
	var objects []storedObject
	paginator := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects in bucket %q: %w", bucket, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, storedObject{
				key:          strings.TrimPrefix(aws.ToString(obj.Key), prefix),
				size:         aws.ToInt64(obj.Size),
				lastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return objects, nil
}

// getStoredObject reads at most limit bytes of an object.
func getStoredObject(ctx context.Context, s3c importS3Client, bucket, key string, limit int64) ([]byte, error) {
	resp, err := s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body close
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// binlogIndexDir returns the storage path whose binary logs the index at key
// describes: key is <dir>/binlog/index.yaml.
func binlogIndexDir(key string) (string, bool) {
	if path.Base(key) != binlogIndexFile {
		return "", false
	}
	binlogDir := path.Dir(key)
	if path.Base(binlogDir) != binlogPathSegment {
		return "", false
	}
	return path.Dir(binlogDir), true
}

// inBinlogDir reports whether key lies in the binlog directory of a storage
// path, where only archived binary logs and their index are kept.
func inBinlogDir(key string) bool {
	return slices.Contains(strings.Split(path.Dir(key), "/"), binlogPathSegment)
}

// importedBackups builds an external Backup for every backup file in objects,
// sorted by path. versions maps a storage path to the engine version its
// binary logs record.
func importedBackups(
	imp *backupv1alpha1.BackupImport,
	objects []storedObject,
	versions map[string]string,
) []*backupv1alpha1.Backup {
	var backups []*backupv1alpha1.Backup
	for _, obj := range objects {
		if inBinlogDir(obj.key) {
			continue
		}
		backupType, startedAt, ok := parseBackupFileName(path.Base(obj.key))
		if !ok {
			continue
		}
		backups = append(backups, importedBackup(imp, obj, backupType, startedAt, versions[path.Dir(obj.key)]))
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Spec.Origin.External.Path < backups[j].Spec.Origin.External.Path
	})
	return backups
}

// importedBackup builds the external Backup of one backup file.
//
// Its path is the file's key, so each file is a Backup of its own even though
// the operator keeps every backup of a storage path in one directory: seeding
// reads the directory and pins the file with its timestamp. The file name
// carries when the backup started; it is complete once the object is written.
func importedBackup(
	imp *backupv1alpha1.BackupImport,
	obj storedObject,
	backupType string,
	startedAt time.Time,
	engineVersion string,
) *backupv1alpha1.Backup {
	completedAt := obj.lastModified
	if completedAt.Before(startedAt) {
		completedAt = startedAt
	}
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:        importBackupName(imp.Spec.StorageRef.Name, obj.key),
			Namespace:   imp.Namespace,
			Labels:      sourceLabels(path.Dir(obj.key)),
			Annotations: map[string]string{sizeAnnotation: strconv.FormatInt(obj.size, 10)},
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeExternal,
				External: &backupv1alpha1.BackupOriginExternal{
					Path:        obj.key,
					StartedAt:   metav1.NewTime(startedAt),
					CompletedAt: metav1.NewTime(completedAt),
					SourceMetadata: &backupv1alpha1.BackupSourceMetadata{
						EngineVersion: engineVersion,
						BackupType:    backupType,
					},
				},
			},
			ClassRef:       imp.Spec.ClassRef,
			StorageRef:     imp.Spec.StorageRef,
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyRetain,
			// The type tells seeding which kind of backup to restore, as for
			// the Backups the provider takes.
			Parameters: &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"type":%q}`, backupType))},
		},
	}
}

// sourceLabels returns the labels naming the Instance and namespace a backup
// was taken from, read from its storage path when that is laid out as the
// runtime writes it. Backups kept elsewhere get none.
func sourceLabels(dir string) map[string]string {
	segments := strings.Split(dir, "/")
	if len(segments) != 5 || segments[0] != backupsPathSegment {
		return nil
	}
	namespace, instance := segments[1], segments[2]
	if len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Label(instance)) > 0 {
		return nil
	}
	return map[string]string{
		sourceNamespaceLabel: namespace,
		sourceInstanceLabel:  instance,
	}
}

// parseBackupFileName reports whether name is a backup file the operator
// writes, of which type, and the time in its name. Logical backups are named
// backup.<RFC 3339 time>.sql, optionally compressed (backup.<time>.sql.gz, or
// the legacy backup.<time>.gzip.sql); physical ones are named
// physicalbackup-<YYYYMMDDHHMMSS>.xb, optionally with a compression extension.
func parseBackupFileName(name string) (string, time.Time, bool) {
	switch {
	case strings.HasPrefix(name, logicalBackupPrefix):
		t, ok := parseLogicalBackupFileName(name)
		return backupTypeLogical, t, ok
	case strings.HasPrefix(name, physicalBackupPrefix):
		t, ok := parsePhysicalBackupFileName(name)
		return backupTypePhysical, t, ok
	default:
		return "", time.Time{}, false
	}
}

func parseLogicalBackupFileName(name string) (time.Time, bool) {
	parts := strings.Split(name, ".")
	switch {
	case len(parts) == 3 && parts[2] == "sql":
	case len(parts) == 4 && parts[2] == "sql":
		if _, err := mariadbv1alpha1.CompressionFromExtension(parts[3]); err != nil {
			return time.Time{}, false
		}
	case len(parts) == 4 && parts[3] == "sql":
		if err := mariadbv1alpha1.CompressAlgorithm(parts[2]).Validate(); err != nil {
			return time.Time{}, false
		}
	default:
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func parsePhysicalBackupFileName(name string) (time.Time, bool) {
	parts := strings.Split(strings.TrimPrefix(name, physicalBackupPrefix), ".")
	switch {
	case len(parts) == 2 && parts[1] == "xb":
	case len(parts) == 3 && parts[1] == "xb":
		if _, err := mariadbv1alpha1.CompressionFromExtension(parts[2]); err != nil {
			return time.Time{}, false
		}
	default:
		return time.Time{}, false
	}
	t, err := time.Parse(physicalBackupLayout, parts[0])
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// importBackupName derives a deterministic Backup CR name from the (storage,
// path) pair so re-running discovery never creates duplicates. The name is
// "import-" followed by a 64-bit FNV-1a hash rendered as 16 hex digits.
func importBackupName(storageName, key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(storageName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("import-%016x", h.Sum64())
}

// isImported reports whether backup was imported from a storage rather than
// taken by the provider.
func isImported(backup *backupv1alpha1.Backup) bool {
	return backup.Spec.Origin.External != nil
}

// namesBackupFile reports whether backup is imported with a path naming one
// backup file, as a BackupImport imports them, rather than the directory
// holding its files.
func namesBackupFile(backup *backupv1alpha1.Backup) bool {
	if !isImported(backup) {
		return false
	}
	_, _, ok := parseBackupFileName(path.Base(backup.Spec.Origin.External.Path))
	return ok
}
