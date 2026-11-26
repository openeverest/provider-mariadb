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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// fakeImportS3 serves a fixed set of objects, pageSize keys per listing page.
type fakeImportS3 struct {
	objects  map[string]string
	modified time.Time
	pageSize int
	listed   int
}

func (f *fakeImportS3) ListObjectsV2(
	_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	f.listed++
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if in.ContinuationToken != nil {
		start, _ = strconv.Atoi(*in.ContinuationToken)
	}
	end := min(start+f.pageSize, len(keys))
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, s3types.Object{
			Key:          aws.String(k),
			Size:         aws.Int64(int64(len(f.objects[k]))),
			LastModified: aws.Time(f.modified),
		})
	}
	if end < len(keys) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeImportS3) GetObject(
	_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, errors.New("no such key")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewBufferString(data))}, nil
}

func useFakeImportS3(t *testing.T, f *fakeImportS3) {
	t.Helper()
	orig := newImportS3Client
	newImportS3Client = func(context.Context, client.Client, *backupv1alpha1.BackupStorage) (importS3Client, error) {
		return f, nil
	}
	t.Cleanup(func() { newImportS3Client = orig })
}

func testBackupImport() *backupv1alpha1.BackupImport {
	return &backupv1alpha1.BackupImport{
		ObjectMeta: metav1.ObjectMeta{Name: "dr", Namespace: "ns"},
		Spec: backupv1alpha1.BackupImportSpec{
			ClassRef:   commonv1alpha1.ObjectRef{Name: backupClassName},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
		},
	}
}

func TestParseBackupFileName(t *testing.T) {
	at := time.Date(2026, 2, 20, 18, 0, 4, 0, time.UTC)
	tests := []struct {
		name     string
		wantType string
		wantOK   bool
	}{
		{"backup.2026-02-20T18:00:04Z.sql", backupTypeLogical, true},
		{"backup.2026-02-20T18:00:04Z.sql.gz", backupTypeLogical, true},
		{"backup.2026-02-20T18:00:04Z.sql.bz2", backupTypeLogical, true},
		{"backup.2026-02-20T18:00:04Z.sql.zst", backupTypeLogical, true},
		{"backup.2026-02-20T18:00:04Z.gzip.sql", backupTypeLogical, true},
		{"backup.2026-02-20T20:00:04+02:00.sql", backupTypeLogical, true},
		{"physicalbackup-20260220180004.xb", backupTypePhysical, true},
		{"physicalbackup-20260220180004.xb.gz", backupTypePhysical, true},
		{"backup.2026-02-20T18:00:04Z.sql.tar", backupTypeLogical, false},
		{"backup.yesterday.sql", backupTypeLogical, false},
		{"backup.2026-02-20T18:00:04Z", backupTypeLogical, false},
		{"physicalbackup-2026.xb", backupTypePhysical, false},
		{"physicalbackup-20260220180004.tar", backupTypePhysical, false},
		{"mariadb-bin.000001", "", false},
		{"index.yaml", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotTime, ok := parseBackupFileName(tt.name)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantType, gotType)
				assert.True(t, at.Equal(gotTime), "time %s", gotTime)
				assert.Equal(t, time.UTC, gotTime.Location())
			}
		})
	}
}

func TestImportBackups(t *testing.T) {
	const dir = "team/backups/ns/src/uid-src/s3"
	modified := time.Date(2026, 2, 20, 18, 30, 0, 0, time.UTC)
	f := &fakeImportS3{
		modified: modified,
		pageSize: 2,
		objects: map[string]string{
			dir + "/backup.2026-02-20T18:00:04Z.sql.gz":      "logical",
			dir + "/physicalbackup-20260220180104.xb":        "physical!",
			dir + "/notes.txt":                               "ignored",
			dir + "/binlog/server-0/mariadb-bin.000001":      "binlog",
			dir + "/binlog/physicalbackup-20260101000000.xb": "not a backup",
			dir + "/binlog/index.yaml": `apiVersion: v1
binlogs:
  server-0:
  - serverVersion: 11.4.4-MariaDB-log
    binlogFilename: mariadb-bin.000001
    lastTime: "2026-02-20T18:10:00Z"
  - serverVersion: 11.8.2-MariaDB-log
    binlogFilename: mariadb-bin.000002
    lastTime: "2026-02-20T18:20:00Z"
`,
			"team/old/backup.2026-02-20T18:00:04Z.sql":                "elsewhere",
			"other/backups/ns/x/u/s3/backup.2026-02-20T18:00:04Z.sql": "outside the prefix",
		},
	}
	useFakeImportS3(t, f)
	p := &MariaDBProvider{client: fake.NewClientBuilder().Build()}

	out, err := p.ImportBackups(context.Background(), testBackupImport(), s3BackupStorage("s3", "https://minio:9000"))
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupImportStateSucceeded, out.State)
	assert.Greater(t, f.listed, 1, "listing is paginated")

	var paths []string
	for _, b := range out.Backups {
		paths = append(paths, b.Spec.Origin.External.Path)
	}
	require.Equal(t, []string{
		"backups/ns/src/uid-src/s3/backup.2026-02-20T18:00:04Z.sql.gz",
		"backups/ns/src/uid-src/s3/physicalbackup-20260220180104.xb",
		"old/backup.2026-02-20T18:00:04Z.sql",
	}, paths)

	logical, physical, elsewhere := out.Backups[0], out.Backups[1], out.Backups[2]
	assert.Equal(t, importBackupName("s3", paths[0]), logical.Name)
	assert.NotEqual(t, logical.Name, physical.Name)
	assert.Equal(t, map[string]string{sourceInstanceLabel: "src", sourceNamespaceLabel: "ns"}, physical.Labels)
	assert.Empty(t, elsewhere.Labels)
	assert.Equal(t, "9", physical.Annotations[sizeAnnotation])

	assert.Equal(t, backupv1alpha1.BackupOriginTypeExternal, physical.Spec.Origin.Type)
	assert.True(t, time.Date(2026, 2, 20, 18, 1, 4, 0, time.UTC).Equal(physical.Spec.Origin.External.StartedAt.Time))
	assert.True(t, modified.Equal(physical.Spec.Origin.External.CompletedAt.Time))
	assert.Equal(t, "11.8.2-MariaDB-log", physical.Spec.Origin.External.SourceMetadata.EngineVersion)
	assert.Equal(t, backupTypePhysical, physical.Spec.Origin.External.SourceMetadata.BackupType)
	assert.Empty(t, elsewhere.Spec.Origin.External.SourceMetadata.EngineVersion)
	assert.Equal(t, backupv1alpha1.BackupDeletionPolicyRetain, physical.Spec.DeletionPolicy)
	assert.Equal(t, "s3", physical.Spec.StorageRef.Name)
	assert.Equal(t, backupClassName, physical.Spec.ClassRef.Name)

	params, err := parseBackupParams(rawParameters(physical.Spec.Parameters))
	require.NoError(t, err)
	assert.True(t, isPhysical(params))
	params, err = parseBackupParams(rawParameters(logical.Spec.Parameters))
	require.NoError(t, err)
	assert.False(t, isPhysical(params))

	again, err := p.ImportBackups(context.Background(), testBackupImport(), s3BackupStorage("s3", "https://minio:9000"))
	require.NoError(t, err)
	assert.Equal(t, logical.Name, again.Backups[0].Name, "names are deterministic")
}

func TestImportBackupsWithoutObjectStoreFails(t *testing.T) {
	storage := s3BackupStorage("s3", "https://minio:9000")
	storage.Spec.Location.ObjectStore = nil
	p := &MariaDBProvider{client: fake.NewClientBuilder().Build()}

	out, err := p.ImportBackups(context.Background(), testBackupImport(), storage)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupImportStateFailed, out.State)
}

func TestSourceLabels(t *testing.T) {
	assert.Equal(t, map[string]string{sourceInstanceLabel: "db", sourceNamespaceLabel: "ns"},
		sourceLabels("backups/ns/db/uid/s3"))
	assert.Nil(t, sourceLabels("backups/ns/db/uid"))
	assert.Nil(t, sourceLabels("other/ns/db/uid/s3"))
	assert.Nil(t, sourceLabels("backups/ns/Not_A_Name/uid/s3"))
}

// importedBackupOf is a Backup as a BackupImport imports it from the storage
// path of the "src" Instance.
func importedBackupOf(name, backupType, file string, startedAt time.Time) *backupv1alpha1.Backup {
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{sourceInstanceLabel: "src", sourceNamespaceLabel: "ns"},
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeExternal,
				External: &backupv1alpha1.BackupOriginExternal{
					Path:        "backups/ns/src/uid-src/s3/" + file,
					StartedAt:   metav1.NewTime(startedAt),
					CompletedAt: metav1.NewTime(startedAt.Add(time.Minute)),
				},
			},
			ClassRef:   commonv1alpha1.ObjectRef{Name: backupClassName},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
			Parameters: &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"type":%q}`, backupType))},
		},
		Status: backupv1alpha1.BackupStatus{
			State:       backupv1alpha1.BackupStateSucceeded,
			CompletedAt: &metav1.Time{Time: startedAt.Add(time.Minute)},
		},
	}
}

func TestBackupS3ImportedFile(t *testing.T) {
	at := time.Date(2026, 2, 20, 18, 0, 4, 0, time.UTC)
	backup := importedBackupOf("imp", backupTypeLogical, "backup.2026-02-20T18:00:04Z.sql", at)
	c := newContextWith(t, s3BackupStorage("s3", "https://minio:9000"))

	got, err := backupS3(c, backup)
	require.NoError(t, err)
	assert.Equal(t, "team/backups/ns/src/uid-src/s3", got.Prefix)
}

func TestResolveBootstrapFromImportedBackup(t *testing.T) {
	at := time.Date(2026, 2, 20, 18, 1, 4, 0, time.UTC)
	backup := importedBackupOf("imp", backupTypePhysical, "physicalbackup-20260220180104.xb", at)
	c := newContextForInstance(t, instanceSeededFrom("imp"), backup, s3BackupStorage("s3", "https://minio:9000"))

	bootstrap, sourceInstance, err := resolveBootstrapFrom(c)
	require.NoError(t, err)
	assert.Equal(t, "src", sourceInstance)
	assert.Equal(t, mariadbv1alpha1.BackupContentTypePhysical, bootstrap.BackupContentType)
	assert.Equal(t, "team/backups/ns/src/uid-src/s3", bootstrap.S3.Prefix)
	require.NotNil(t, bootstrap.TargetRecoveryTime)
	assert.True(t, at.Equal(bootstrap.TargetRecoveryTime.Time), "pinned to the file's time")

	t.Run("without a source label the source is unknown", func(t *testing.T) {
		backup := importedBackupOf("imp", backupTypeLogical, "backup.2026-02-20T18:00:04Z.sql", at)
		backup.Labels = nil
		c := newContextForInstance(t, instanceSeededFrom("imp"), backup, s3BackupStorage("s3", "https://minio:9000"))

		_, sourceInstance, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		assert.Empty(t, sourceInstance)
	})
}

func TestSyncRestoreRejectsImportedBackup(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	backup := importedBackupOf("imp", backupTypeLogical, "backup.2026-02-20T18:00:04Z.sql", time.Now())
	backup.Labels[sourceInstanceLabel] = "db"
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1", Namespace: "ns"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "db"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "imp"}},
			},
		},
	}
	c := newContextWith(t, mdb, backup, restore)

	out, err := (&MariaDBProvider{}).SyncRestore(c, restore)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.RestoreStateFailed, out.State)
	assert.Contains(t, out.Message, "spec.dataSource")
	assert.True(t, controller.IsNotFound(c.Get(&mariadbv1alpha1.Restore{}, "restore-1")))
}

func TestResolvePointInTimeBootstrapFromImport(t *testing.T) {
	t1 := time.Date(2026, 2, 20, 18, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 21, 18, 0, 0, 0, time.UTC)
	older := importedBackupOf("older", backupTypePhysical, "physicalbackup-20260220180000.xb", t1)
	older.Spec.Origin.External.Path = "backups/ns/src/uid-old/s3/physicalbackup-20260220180000.xb"
	newer := importedBackupOf("newer", backupTypePhysical, "physicalbackup-20260221180000.xb", t2)
	logical := importedBackupOf("logical", backupTypeLogical, "backup.2026-02-21T19:00:00Z.sql", t2.Add(time.Hour))
	onOther := importedBackupOf("other", backupTypePhysical, "physicalbackup-20260221190000.xb", t2.Add(time.Hour))
	onOther.Spec.StorageRef.Name = "elsewhere"
	storage := s3BackupStorage("s3", "https://minio:9000")
	imports := []client.Object{storage, older, newer, logical, onOther}

	t.Run("date target recovers from the latest base before it", func(t *testing.T) {
		date := metav1.NewTime(t1.Add(time.Hour))
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetDate, &date)
		c := newPITRContext(t, in, imports...)

		bootstrap, sourceInstance, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		assert.Equal(t, "src", sourceInstance)
		name := importedPITRName("db")
		require.NotNil(t, bootstrap.PointInTimeRecoveryRef)
		assert.Equal(t, name, bootstrap.PointInTimeRecoveryRef.Name)
		assert.Equal(t, mariadbv1alpha1.BackupContentTypePhysical, bootstrap.BackupContentType)
		assert.True(t, date.Equal(bootstrap.TargetRecoveryTime))

		physical := &mariadbv1alpha1.PhysicalBackup{}
		require.NoError(t, c.Get(physical, name))
		assert.Equal(t, "team/backups/ns/src/uid-old/s3", physical.Spec.Storage.S3.Prefix)
		require.NotNil(t, physical.Spec.Schedule)
		assert.True(t, physical.Spec.Schedule.Suspend)
		assert.Empty(t, physical.Spec.Schedule.Cron)
		assert.Equal(t, "db", physical.Spec.MariaDBRef.Name)
		assert.NotContains(t, physical.Labels, instanceLabel, "never mirrored as a scheduled backup")
		assert.Equal(t, "db", metav1.GetControllerOf(physical).Name)

		pitr := &mariadbv1alpha1.PointInTimeRecovery{}
		require.NoError(t, c.Get(pitr, name))
		assert.Equal(t, name, pitr.Spec.PhysicalBackupRef.Name)
		assert.Equal(t, "team/backups/ns/src/uid-old/s3/binlog", pitr.Spec.PointInTimeRecoveryStorage.S3.Prefix)
		assert.Equal(t, "db", metav1.GetControllerOf(pitr).Name)
	})

	t.Run("latest target recovers from the latest base", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		c := newPITRContext(t, in, imports...)

		bootstrap, _, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		assert.Nil(t, bootstrap.TargetRecoveryTime)
		physical := &mariadbv1alpha1.PhysicalBackup{}
		require.NoError(t, c.Get(physical, importedPITRName("db")))
		assert.Equal(t, "team/backups/ns/src/uid-src/s3", physical.Spec.Storage.S3.Prefix)
	})

	t.Run("a date before every imported base is a data source error", func(t *testing.T) {
		date := metav1.NewTime(t1.Add(-time.Hour))
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetDate, &date)
		c := newPITRContext(t, in, imports...)

		_, _, err := resolveBootstrapFrom(c)
		var dsErr *controller.DataSourceConfigError
		require.ErrorAs(t, err, &dsErr)
		assert.Contains(t, dsErr.Message, "src")
	})

	t.Run("waits for an import when nothing is imported", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		c := newPITRContext(t, in, storage, logical, onOther)

		_, _, err := resolveBootstrapFrom(c)
		assert.True(t, controller.IsWaitError(err))
	})

	fromNamespace := func(name, ns string, at time.Time) *backupv1alpha1.Backup {
		file := "physicalbackup-" + at.Format(physicalBackupLayout) + ".xb"
		b := importedBackupOf(name, backupTypePhysical, file, at)
		b.Labels[sourceNamespaceLabel] = ns
		b.Spec.Origin.External.Path = "backups/" + ns + "/src/uid-" + ns + "/s3/" + file
		return b
	}
	latestBase := func(t *testing.T, objs ...client.Object) (string, error) {
		t.Helper()
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		c := newPITRContext(t, in, append([]client.Object{storage}, objs...)...)
		if _, _, err := resolveBootstrapFrom(c); err != nil {
			return "", err
		}
		physical := &mariadbv1alpha1.PhysicalBackup{}
		require.NoError(t, c.Get(physical, importedPITRName("db")))
		return physical.Spec.Storage.S3.Prefix, nil
	}

	t.Run("a same-named Instance of another namespace is ignored", func(t *testing.T) {
		prefix, err := latestBase(t, newer, fromNamespace("later", "other", t2.Add(time.Hour)))
		require.NoError(t, err)
		assert.Equal(t, "team/backups/ns/src/uid-src/s3", prefix)
	})

	t.Run("the only other namespace is recovered from", func(t *testing.T) {
		prefix, err := latestBase(t, fromNamespace("moved", "old-ns", t2))
		require.NoError(t, err)
		assert.Equal(t, "team/backups/old-ns/src/uid-old-ns/s3", prefix)
	})

	t.Run("several other namespaces are ambiguous", func(t *testing.T) {
		_, err := latestBase(t, fromNamespace("a", "ns-a", t1), fromNamespace("b", "ns-b", t2))
		var dsErr *controller.DataSourceConfigError
		require.ErrorAs(t, err, &dsErr)
		assert.Contains(t, dsErr.Message, "ns-a, ns-b")
	})

	t.Run("the source PITR CR is preferred while it exists", func(t *testing.T) {
		in := instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil)
		sourcePITR := &mariadbv1alpha1.PointInTimeRecovery{
			ObjectMeta: metav1.ObjectMeta{Name: pitrCRName("src"), Namespace: "ns"},
		}
		c := newPITRContext(t, in, append(imports, sourcePITR)...)

		bootstrap, _, err := resolveBootstrapFrom(c)
		require.NoError(t, err)
		assert.Equal(t, pitrCRName("src"), bootstrap.PointInTimeRecoveryRef.Name)
	})
}

func TestEnsureRestoreCredentialsOfDeletedSource(t *testing.T) {
	t.Run("pre-created target secrets are used", func(t *testing.T) {
		c := newContextWith(t,
			credentialSecret(rootSecretName("db"), rootPasswordSecretKey, "r00t"),
			credentialSecret(userSecretName("db"), userPasswordSecretKey, "s3cret"))

		require.NoError(t, ensureRestoreCredentials(c, "gone"))
		require.NoError(t, ensureRestoreCredentials(c, ""))
	})

	t.Run("the error names the secret to create", func(t *testing.T) {
		for _, source := range []string{"gone", ""} {
			c := newContextWith(t)

			err := ensureRestoreCredentials(c, source)
			var dsErr *controller.DataSourceConfigError
			require.ErrorAs(t, err, &dsErr)
			assert.Contains(t, dsErr.Message, rootSecretName("db"))
			assert.Contains(t, dsErr.Message, rootPasswordSecretKey)
			assert.True(t, controller.IsNotFound(c.Get(&corev1.Secret{}, rootSecretName("db"))))
		}
	})
}
