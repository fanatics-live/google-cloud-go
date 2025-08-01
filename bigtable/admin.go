/*
Copyright 2015 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bigtable

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btopt "cloud.google.com/go/bigtable/internal/option"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/internal/optional"
	"cloud.google.com/go/longrunning"
	lroauto "cloud.google.com/go/longrunning/autogen"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	field_mask "google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UNIVERSE_DOMAIN placeholder is replaced by the UniverseDomain from DialSettings while creating GRPC connection/dial pool.
const adminAddr = "bigtableadmin.UNIVERSE_DOMAIN:443"
const mtlsAdminAddr = "bigtableadmin.mtls.googleapis.com:443"

var (
	errExpiryMissing  = errors.New("WithExpiry is a required option")
	adminRetryOptions = []gax.CallOption{
		gax.WithRetry(func() gax.Retryer {
			return &bigtableAdminRetryer{
				Backoff: defaultBackoff,
			}
		}),
	}
)

// bigtableAdminRetryer extends the generic gax Retryer, but also checks
// error messages to check if operation can be retried
//
// Retry is made if :
// - error code is one of the `idempotentRetryCodes` OR
// - error code is internal and error message is one of the `retryableInternalErrMsgs`
type bigtableAdminRetryer struct {
	gax.Backoff
}

func (r *bigtableAdminRetryer) Retry(err error) (time.Duration, bool) {
	// Similar to gax.OnCodes but shares the backoff with INTERNAL retry messages check
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return 0, false
	}
	c := st.Code()
	_, isIdempotent := isIdempotentRetryCode[c]
	if isIdempotent ||
		(grpcstatus.Code(err) == codes.Internal && containsAny(err.Error(), retryableInternalErrMsgs)) {
		pause := r.Backoff.Pause()
		return pause, true
	}
	return 0, false
}

// ErrPartiallyUnavailable is returned when some locations (clusters) are
// unavailable. Both partial results (retrieved from available locations)
// and the error are returned when this exception occurred.
type ErrPartiallyUnavailable struct {
	Locations []string // unavailable locations
}

func (e ErrPartiallyUnavailable) Error() string {
	return fmt.Sprintf("Unavailable locations: %v", e.Locations)
}

// AdminClient is a client type for performing admin operations within a specific instance.
type AdminClient struct {
	connPool  gtransport.ConnPool
	tClient   btapb.BigtableTableAdminClient
	lroClient *lroauto.OperationsClient

	project, instance string

	// Metadata to be sent with each request.
	md metadata.MD
}

// NewAdminClient creates a new AdminClient for a given project and instance.
func NewAdminClient(ctx context.Context, project, instance string, opts ...option.ClientOption) (*AdminClient, error) {
	o, err := btopt.DefaultClientOptions(adminAddr, mtlsAdminAddr, AdminScope, clientUserAgent)
	if err != nil {
		return nil, err
	}
	// Add gRPC client interceptors to supply Google client information. No external interceptors are passed.
	o = append(o, btopt.ClientInterceptorOptions(nil, nil)...)
	// Need to add scopes for long running operations (for create table & snapshots)
	o = append(o, option.WithScopes(cloudresourcemanager.CloudPlatformScope))
	o = append(o, opts...)
	connPool, err := gtransport.DialPool(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	lroClient, err := lroauto.NewOperationsClient(ctx, gtransport.WithConnPool(connPool))
	if err != nil {
		// This error "should not happen", since we are just reusing old connection
		// and never actually need to dial.
		// If this does happen, we could leak conn. However, we cannot close conn:
		// If the user invoked the function with option.WithGRPCConn,
		// we would close a connection that's still in use.
		// TODO(pongad): investigate error conditions.
		return nil, err
	}

	return &AdminClient{
		connPool:  connPool,
		tClient:   btapb.NewBigtableTableAdminClient(connPool),
		lroClient: lroClient,
		project:   project,
		instance:  instance,
		md:        metadata.Pairs(resourcePrefixHeader, fmt.Sprintf("projects/%s/instances/%s", project, instance)),
	}, nil
}

// Close closes the AdminClient.
func (ac *AdminClient) Close() error {
	return ac.connPool.Close()
}

func (ac *AdminClient) instancePrefix() string {
	return instancePrefix(ac.project, ac.instance)
}

func instancePrefix(project, instance string) string {
	return fmt.Sprintf("projects/%s/instances/%s", project, instance)
}

func (ac *AdminClient) backupPath(cluster, instance, backup string) string {
	return fmt.Sprintf("projects/%s/instances/%s/clusters/%s/backups/%s", ac.project, instance, cluster, backup)
}

func (ac *AdminClient) authorizedViewPath(table, authorizedView string) string {
	return fmt.Sprintf("%s/tables/%s/authorizedViews/%s", ac.instancePrefix(), table, authorizedView)
}

func (ac *AdminClient) schemaBundlePath(table, schemaBundle string) string {
	return fmt.Sprintf("%s/tables/%s/schemaBundles/%s", ac.instancePrefix(), table, schemaBundle)
}

func logicalViewPath(project, instance, logicalView string) string {
	return fmt.Sprintf("%s/logicalViews/%s", instancePrefix(project, instance), logicalView)
}

func materializedlViewPath(project, instance, materializedView string) string {
	return fmt.Sprintf("%s/materializedViews/%s", instancePrefix(project, instance), materializedView)
}

func appProfilePath(project, instance, appProfile string) string {
	return fmt.Sprintf("%s/appProfiles/%s", instancePrefix(project, instance), appProfile)
}

// EncryptionInfo represents the encryption info of a table.
type EncryptionInfo struct {
	Status        *Status
	Type          EncryptionType
	KMSKeyVersion string
}

func newEncryptionInfo(pbInfo *btapb.EncryptionInfo) *EncryptionInfo {
	return &EncryptionInfo{
		Status:        pbInfo.EncryptionStatus,
		Type:          EncryptionType(pbInfo.EncryptionType.Number()),
		KMSKeyVersion: pbInfo.KmsKeyVersion,
	}
}

// Status references google.golang.org/grpc/status.
// It represents an RPC status code, message, and details of EncryptionInfo.
// https://pkg.go.dev/google.golang.org/grpc/internal/status
type Status = status.Status

// EncryptionType is the type of encryption for an instance.
type EncryptionType int32

const (
	// EncryptionTypeUnspecified is the type was not specified, though data at rest remains encrypted.
	EncryptionTypeUnspecified EncryptionType = iota
	// GoogleDefaultEncryption represents that data backing this resource is
	// encrypted at rest with a key that is fully managed by Google. No key
	// version or status will be populated. This is the default state.
	GoogleDefaultEncryption
	// CustomerManagedEncryption represents that data backing this resource is
	// encrypted at rest with a key that is managed by the customer.
	// The in-use version of the key and its status are populated for
	// CMEK-protected tables.
	// CMEK-protected backups are pinned to the key version that was in use at
	// the time the backup was taken. This key version is populated but its
	// status is not tracked and is reported as `UNKNOWN`.
	CustomerManagedEncryption
)

// EncryptionInfoByCluster is a map of cluster name to EncryptionInfo
type EncryptionInfoByCluster map[string][]*EncryptionInfo

// EncryptionInfo gets the current encryption info for the table across all of the clusters.
// The returned map will be keyed by cluster id and contain a status for all of the keys in use.
func (ac *AdminClient) EncryptionInfo(ctx context.Context, table string) (EncryptionInfoByCluster, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)

	res, err := ac.getTable(ctx, table, btapb.Table_ENCRYPTION_VIEW)
	if err != nil {
		return nil, err
	}
	encryptionInfo := EncryptionInfoByCluster{}
	for key, cs := range res.ClusterStates {
		for _, pbInfo := range cs.EncryptionInfo {
			info := EncryptionInfo{}
			info.Status = pbInfo.EncryptionStatus
			info.Type = EncryptionType(pbInfo.EncryptionType.Number())
			info.KMSKeyVersion = pbInfo.KmsKeyVersion
			encryptionInfo[key] = append(encryptionInfo[key], &info)
		}
	}

	return encryptionInfo, nil
}

// Tables returns a list of the tables in the instance.
func (ac *AdminClient) Tables(ctx context.Context) ([]string, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ListTablesRequest{
		Parent: prefix,
	}

	var res *btapb.ListTablesResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.ListTables(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(res.Tables))
	for _, tbl := range res.Tables {
		names = append(names, strings.TrimPrefix(tbl.Name, prefix+"/tables/"))
	}
	return names, nil
}

// ChangeStreamRetention indicates how long bigtable should retain change data.
// Minimum is 1 day. Maximum is 7. nil to not change the retention period. 0 to
// disable change stream retention.
type ChangeStreamRetention optional.Duration

// DeletionProtection indicates whether the table, authorized view, logical view or
// materialized view is protected against data loss i.e. when set to protected,
// deleting the view, the table, the column families in the table,
// and the instance containing the table or view would be prohibited.
type DeletionProtection int

// None indicates that deletion protection is unset
// Protected indicates that deletion protection is enabled
// Unprotected indicates that deletion protection is disabled
const (
	None DeletionProtection = iota
	Protected
	Unprotected
)

// TableAutomatedBackupConfig generalizes automated backup configurations.
// Currently, the only supported type of automated backup configuration
// is TableAutomatedBackupPolicy.
type TableAutomatedBackupConfig interface {
	isTableAutomatedBackupConfig()
}

// TableAutomatedBackupPolicy defines an automated backup policy for a table.
// Use nil TableAutomatedBackupPolicy to disable Automated Backups on a table.
// Use nil for a specific field to ignore that field when updating the policy on a table.
type TableAutomatedBackupPolicy struct {
	// How long the automated backups should be retained. The only
	// supported value at this time is 3 days.
	RetentionPeriod optional.Duration
	// How frequently automated backups should occur. The only
	// supported value at this time is 24 hours.
	Frequency optional.Duration
}

func (*TableAutomatedBackupPolicy) isTableAutomatedBackupConfig() {}

func toAutomatedBackupConfigProto(automatedBackupConfig TableAutomatedBackupConfig) (*btapb.Table_AutomatedBackupPolicy_, error) {
	if automatedBackupConfig == nil {
		return nil, nil
	}
	switch backupConfig := automatedBackupConfig.(type) {
	case *TableAutomatedBackupPolicy:
		return backupConfig.toProto()
	default:
		return nil, fmt.Errorf("error: Unknown type of automated backup configuration")
	}
}

func (abp *TableAutomatedBackupPolicy) toProto() (*btapb.Table_AutomatedBackupPolicy_, error) {
	pbAutomatedBackupPolicy := &btapb.Table_AutomatedBackupPolicy{
		RetentionPeriod: durationpb.New(0),
		Frequency:       durationpb.New(0),
	}
	if abp.RetentionPeriod == nil && abp.Frequency == nil {
		return nil, errors.New("at least one of RetentionPeriod and Frequency must be set")
	}
	if abp.RetentionPeriod != nil {
		pbAutomatedBackupPolicy.RetentionPeriod = durationpb.New(optional.ToDuration(abp.RetentionPeriod))
	}
	if abp.Frequency != nil {
		pbAutomatedBackupPolicy.Frequency = durationpb.New(optional.ToDuration(abp.Frequency))
	}
	return &btapb.Table_AutomatedBackupPolicy_{
		AutomatedBackupPolicy: pbAutomatedBackupPolicy,
	}, nil
}

// Family represents a column family with its optional GC policy and value type.
type Family struct {
	GCPolicy  GCPolicy
	ValueType Type
}

// UpdateTableConf is unused
type UpdateTableConf struct{}

// TableConf contains all the information necessary to create a table with column families.
type TableConf struct {
	TableID   string
	SplitKeys []string
	// DEPRECATED: Use ColumnFamilies instead.
	// Families is a map from family name to GCPolicy.
	// Only one of Families or ColumnFamilies may be set.
	Families map[string]GCPolicy
	// ColumnFamilies is a map from family name to family configuration.
	// Only one of Families or ColumnFamilies may be set.
	ColumnFamilies map[string]Family
	// DeletionProtection can be none, protected or unprotected
	// set to protected to make the table protected against data loss
	DeletionProtection    DeletionProtection
	ChangeStreamRetention ChangeStreamRetention
	// Configure an automated backup policy for the table
	AutomatedBackupConfig TableAutomatedBackupConfig
	// Configure a row key schema for the table
	RowKeySchema *StructType
}

// CreateTable creates a new table in the instance.
// This method may return before the table's creation is complete.
func (ac *AdminClient) CreateTable(ctx context.Context, table string) error {
	return ac.CreateTableFromConf(ctx, &TableConf{TableID: table, ChangeStreamRetention: nil, DeletionProtection: None})
}

// CreatePresplitTable creates a new table in the instance.
// The list of row keys will be used to initially split the table into multiple tablets.
// Given two split keys, "s1" and "s2", three tablets will be created,
// spanning the key ranges: [, s1), [s1, s2), [s2, ).
// This method may return before the table's creation is complete.
func (ac *AdminClient) CreatePresplitTable(ctx context.Context, table string, splitKeys []string) error {
	return ac.CreateTableFromConf(ctx, &TableConf{TableID: table, SplitKeys: splitKeys, ChangeStreamRetention: nil, DeletionProtection: None})
}

// CreateTableFromConf creates a new table in the instance from the given configuration.
func (ac *AdminClient) CreateTableFromConf(ctx context.Context, conf *TableConf) error {
	if conf.TableID == "" {
		return errors.New("TableID is required")
	}
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	var reqSplits []*btapb.CreateTableRequest_Split
	for _, split := range conf.SplitKeys {
		reqSplits = append(reqSplits, &btapb.CreateTableRequest_Split{Key: []byte(split)})
	}
	var tbl btapb.Table
	// we'd rather not set anything explicitly if users don't specify a value and let the server set the default value.
	// if DeletionProtection is not set, currently the API will default it to false.
	if conf.DeletionProtection == Protected {
		tbl.DeletionProtection = true
	} else if conf.DeletionProtection == Unprotected {
		tbl.DeletionProtection = false
	}
	if conf.ChangeStreamRetention != nil && conf.ChangeStreamRetention.(time.Duration) != 0 {
		tbl.ChangeStreamConfig = &btapb.ChangeStreamConfig{}
		tbl.ChangeStreamConfig.RetentionPeriod = durationpb.New(conf.ChangeStreamRetention.(time.Duration))
	}

	if conf.AutomatedBackupConfig != nil {
		proto, err := toAutomatedBackupConfigProto(conf.AutomatedBackupConfig)
		if err != nil {
			return err
		}
		tbl.AutomatedBackupConfig = proto
	}

	if conf.RowKeySchema != nil {
		tbl.RowKeySchema = conf.RowKeySchema.proto().GetStructType()
	}

	if conf.Families != nil && conf.ColumnFamilies != nil {
		return errors.New("only one of Families or ColumnFamilies may be set, not both")
	}

	if conf.ColumnFamilies != nil {
		tbl.ColumnFamilies = make(map[string]*btapb.ColumnFamily)
		for fam, config := range conf.ColumnFamilies {
			var gcPolicy *btapb.GcRule
			if config.GCPolicy != nil {
				gcPolicy = config.GCPolicy.proto()
			} else {
				gcPolicy = &btapb.GcRule{}
			}

			var typeProto *btapb.Type = nil
			if config.ValueType != nil {
				typeProto = config.ValueType.proto()
			}

			tbl.ColumnFamilies[fam] = &btapb.ColumnFamily{GcRule: gcPolicy, ValueType: typeProto}
		}
	} else if conf.Families != nil {
		tbl.ColumnFamilies = make(map[string]*btapb.ColumnFamily)
		for fam, policy := range conf.Families {
			tbl.ColumnFamilies[fam] = &btapb.ColumnFamily{GcRule: policy.proto()}
		}
	}
	prefix := ac.instancePrefix()
	req := &btapb.CreateTableRequest{
		Parent:        prefix,
		TableId:       conf.TableID,
		Table:         &tbl,
		InitialSplits: reqSplits,
	}
	_, err := ac.tClient.CreateTable(ctx, req)
	return err
}

// CreateColumnFamily creates a new column family in a table.
func (ac *AdminClient) CreateColumnFamily(ctx context.Context, table, family string) error {
	// TODO(dsymonds): Permit specifying gcexpr and any other family settings.
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// CreateColumnFamilyWithConfig creates a new column family in a table with an optional GC policy and value type.
func (ac *AdminClient) CreateColumnFamilyWithConfig(ctx context.Context, table, family string, config Family) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()

	cf := &btapb.ColumnFamily{}
	if config.GCPolicy != nil {
		cf.GcRule = config.GCPolicy.proto()
	}
	if config.ValueType != nil {
		cf.ValueType = config.ValueType.proto()
	}

	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: cf},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

const (
	deletionProtectionFieldMask    = "deletion_protection"
	changeStreamConfigFieldMask    = "change_stream_config"
	automatedBackupPolicyFieldMask = "automated_backup_policy"
	retentionPeriodFieldMaskPath   = "retention_period"
	frequencyFieldMaskPath         = "frequency"
	rowKeySchemaMaskPath           = "row_key_schema"
)

func (ac *AdminClient) newUpdateTableRequestProto(tableID string) (*btapb.UpdateTableRequest, error) {
	if tableID == "" {
		return nil, errors.New("TableID is required")
	}
	updateMask := &field_mask.FieldMask{
		Paths: []string{},
	}
	req := &btapb.UpdateTableRequest{
		Table: &btapb.Table{
			Name: ac.instancePrefix() + "/tables/" + tableID,
		},
		UpdateMask: updateMask,
	}
	return req, nil
}

func (ac *AdminClient) updateTableAndWait(ctx context.Context, updateTableRequest *btapb.UpdateTableRequest) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)

	lro, err := ac.tClient.UpdateTable(ctx, updateTableRequest)
	if err != nil {
		return fmt.Errorf("error from update: %w", err)
	}

	var tbl btapb.Table
	op := longrunning.InternalNewOperation(ac.lroClient, lro)
	err = op.Wait(ctx, &tbl)
	if err != nil {
		return fmt.Errorf("error from operation: %v", err)
	}

	return nil
}

// UpdateTableDisableChangeStream updates a table to disable change stream for table ID.
func (ac *AdminClient) UpdateTableDisableChangeStream(ctx context.Context, tableID string) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = []string{changeStreamConfigFieldMask}
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableWithChangeStream updates a table to with the given table ID and change stream config.
func (ac *AdminClient) UpdateTableWithChangeStream(ctx context.Context, tableID string, changeStreamRetention ChangeStreamRetention) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = []string{changeStreamConfigFieldMask + "." + retentionPeriodFieldMaskPath}
	req.Table.ChangeStreamConfig = &btapb.ChangeStreamConfig{}
	req.Table.ChangeStreamConfig.RetentionPeriod = durationpb.New(changeStreamRetention.(time.Duration))
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableWithDeletionProtection updates a table with the given table ID and deletion protection parameter.
func (ac *AdminClient) UpdateTableWithDeletionProtection(ctx context.Context, tableID string, deletionProtection DeletionProtection) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = []string{deletionProtectionFieldMask}
	req.Table.DeletionProtection = deletionProtection != Unprotected
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableDisableAutomatedBackupPolicy updates a table to disable automated backups for table ID.
func (ac *AdminClient) UpdateTableDisableAutomatedBackupPolicy(ctx context.Context, tableID string) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = []string{automatedBackupPolicyFieldMask}
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableWithAutomatedBackupPolicy updates a table to with the given table ID and automated backup policy config.
func (ac *AdminClient) UpdateTableWithAutomatedBackupPolicy(ctx context.Context, tableID string, automatedBackupPolicy TableAutomatedBackupPolicy) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	abc, err := toAutomatedBackupConfigProto(&automatedBackupPolicy)
	if err != nil {
		return err
	}
	// If the AutomatedBackupPolicy is not at least partially specified, or if both fields are 0, then this is an
	// incorrect configuration for updating the table, and should be rejected. Both fields could be zero if (1)
	// they are set to zero, or (2) neither field was set and the policy was constructed using toProto().
	if abc.AutomatedBackupPolicy.RetentionPeriod.Seconds == 0 && abc.AutomatedBackupPolicy.Frequency.Seconds == 0 {
		return errors.New("Invalid automated backup policy. If you're intending to disable automated backups, please use the UpdateTableDisableAutomatedBackupPolicy method instead")
	}
	if abc.AutomatedBackupPolicy.RetentionPeriod.Seconds != 0 {
		// Update Retention Period
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, automatedBackupPolicyFieldMask+"."+retentionPeriodFieldMaskPath)
	}
	if abc.AutomatedBackupPolicy.Frequency.Seconds != 0 {
		// Update Frequency
		req.UpdateMask.Paths = append(req.UpdateMask.Paths, automatedBackupPolicyFieldMask+"."+frequencyFieldMaskPath)
	}
	req.Table.AutomatedBackupConfig = abc
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableWithRowKeySchema updates a table with RowKeySchema.
func (ac *AdminClient) UpdateTableWithRowKeySchema(ctx context.Context, tableID string, rowKeySchema StructType) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = append(req.UpdateMask.Paths, rowKeySchemaMaskPath)
	req.Table.RowKeySchema = rowKeySchema.proto().GetStructType()
	return ac.updateTableAndWait(ctx, req)
}

// UpdateTableRemoveRowKeySchema removes a RowKeySchema from a table.
func (ac *AdminClient) UpdateTableRemoveRowKeySchema(ctx context.Context, tableID string) error {
	req, err := ac.newUpdateTableRequestProto(tableID)
	if err != nil {
		return err
	}
	req.UpdateMask.Paths = append(req.UpdateMask.Paths, rowKeySchemaMaskPath)
	req.IgnoreWarnings = true
	return ac.updateTableAndWait(ctx, req)
}

// DeleteTable deletes a table and all of its data.
func (ac *AdminClient) DeleteTable(ctx context.Context, table string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.DeleteTableRequest{
		Name: prefix + "/tables/" + table,
	}
	_, err := ac.tClient.DeleteTable(ctx, req)
	return err
}

// DeleteColumnFamily deletes a column family in a table and all of its data.
func (ac *AdminClient) DeleteColumnFamily(ctx context.Context, table, family string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:  family,
			Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Drop{Drop: true},
		}},
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// TableInfo represents information about a table.
type TableInfo struct {
	// DEPRECATED - This field is deprecated. Please use FamilyInfos instead.
	Families    []string
	FamilyInfos []FamilyInfo
	// DeletionProtection indicates whether the table is protected against data loss
	// DeletionProtection could be None depending on the table view
	// for example when using NAME_ONLY, the response does not contain DeletionProtection and the value should be None
	DeletionProtection    DeletionProtection
	ChangeStreamRetention ChangeStreamRetention
	AutomatedBackupConfig TableAutomatedBackupConfig
	RowKeySchema          *StructType
}

// FamilyInfo represents information about a column family.
type FamilyInfo struct {
	Name         string
	GCPolicy     string
	FullGCPolicy GCPolicy
	ValueType    Type
}

func (ac *AdminClient) getTable(ctx context.Context, table string, view btapb.Table_View) (*btapb.Table, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.GetTableRequest{
		Name: prefix + "/tables/" + table,
		View: view,
	}

	var res *btapb.Table

	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.GetTable(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TableInfo retrieves information about a table.
func (ac *AdminClient) TableInfo(ctx context.Context, table string) (*TableInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)

	res, err := ac.getTable(ctx, table, btapb.Table_SCHEMA_VIEW)
	if err != nil {
		return nil, err
	}

	ti := &TableInfo{}
	for name, fam := range res.ColumnFamilies {
		ti.Families = append(ti.Families, name)
		ti.FamilyInfos = append(ti.FamilyInfos, FamilyInfo{
			Name:         name,
			GCPolicy:     GCRuleToString(fam.GcRule),
			FullGCPolicy: gcRuleToPolicy(fam.GcRule),
			ValueType:    ProtoToType(fam.ValueType),
		})
	}
	// we expect DeletionProtection to be in the response because Table_SCHEMA_VIEW is being used in this function
	// but when using NAME_ONLY, the response does not contain DeletionProtection and it could be nil
	if res.DeletionProtection == true {
		ti.DeletionProtection = Protected
	} else {
		ti.DeletionProtection = Unprotected
	}
	if res.ChangeStreamConfig != nil && res.ChangeStreamConfig.RetentionPeriod != nil {
		ti.ChangeStreamRetention = res.ChangeStreamConfig.RetentionPeriod.AsDuration()
	}
	if res.AutomatedBackupConfig != nil {
		switch res.AutomatedBackupConfig.(type) {
		case *btapb.Table_AutomatedBackupPolicy_:
			ti.AutomatedBackupConfig = &TableAutomatedBackupPolicy{
				RetentionPeriod: res.GetAutomatedBackupPolicy().GetRetentionPeriod().AsDuration(),
				Frequency:       res.GetAutomatedBackupPolicy().GetFrequency().AsDuration(),
			}
		default:
			return nil, fmt.Errorf("error: Unknown type of automated backup configuration")
		}
	}
	if res.RowKeySchema != nil {
		structType := structProtoToType(res.RowKeySchema).(StructType)
		ti.RowKeySchema = &structType
	}

	return ti, nil
}

type updateFamilyOption struct {
	ignoreWarnings bool
}

// GCPolicyOption is deprecated, kept for backwards compatibility, use UpdateFamilyOption in new code
type GCPolicyOption interface {
	apply(s *updateFamilyOption)
}

// UpdateFamilyOption is the interface to update family settings
type UpdateFamilyOption GCPolicyOption

type ignoreWarnings bool

func (w ignoreWarnings) apply(s *updateFamilyOption) {
	s.ignoreWarnings = bool(w)
}

// IgnoreWarnings returns a updateFamilyOption that ignores safety checks when modifying the column families
func IgnoreWarnings() GCPolicyOption {
	return ignoreWarnings(true)
}

// SetGCPolicy specifies which cells in a column family should be garbage collected.
// GC executes opportunistically in the background; table reads may return data
// matching the GC policy.
func (ac *AdminClient) SetGCPolicy(ctx context.Context, table, family string, policy GCPolicy) error {
	return ac.UpdateFamily(ctx, table, family, Family{GCPolicy: policy})
}

// SetGCPolicyWithOptions is similar to SetGCPolicy but allows passing options
func (ac *AdminClient) SetGCPolicyWithOptions(ctx context.Context, table, family string, policy GCPolicy, opts ...GCPolicyOption) error {
	familyOpts := []UpdateFamilyOption{}
	for _, opt := range opts {
		if opt != nil {
			familyOpts = append(familyOpts, opt.(UpdateFamilyOption))
		}
	}
	return ac.UpdateFamily(ctx, table, family, Family{GCPolicy: policy}, familyOpts...)
}

// UpdateFamily updates column families' garbage collection policies and value type.
func (ac *AdminClient) UpdateFamily(ctx context.Context, table, familyName string, family Family, opts ...UpdateFamilyOption) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()

	s := updateFamilyOption{}
	for _, opt := range opts {
		if opt != nil {
			opt.apply(&s)
		}
	}

	cf := &btapb.ColumnFamily{}
	mask := &field_mask.FieldMask{}
	if family.GCPolicy != nil {
		cf.GcRule = family.GCPolicy.proto()
		mask.Paths = append(mask.Paths, "gc_rule")

	}
	if family.ValueType != nil {
		cf.ValueType = family.ValueType.proto()
		mask.Paths = append(mask.Paths, "value_type")
	}

	// No update
	if len(mask.Paths) == 0 {
		return nil
	}

	req := &btapb.ModifyColumnFamiliesRequest{
		Name: prefix + "/tables/" + table,
		Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
			Id:         familyName,
			Mod:        &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: cf},
			UpdateMask: mask,
		}},
		IgnoreWarnings: s.ignoreWarnings,
	}
	_, err := ac.tClient.ModifyColumnFamilies(ctx, req)
	return err
}

// DropRowRange permanently deletes a row range from the specified table.
func (ac *AdminClient) DropRowRange(ctx context.Context, table, rowKeyPrefix string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.DropRowRangeRequest{
		Name:   prefix + "/tables/" + table,
		Target: &btapb.DropRowRangeRequest_RowKeyPrefix{RowKeyPrefix: []byte(rowKeyPrefix)},
	}
	_, err := ac.tClient.DropRowRange(ctx, req)
	return err
}

// DropAllRows permanently deletes all rows from the specified table.
func (ac *AdminClient) DropAllRows(ctx context.Context, table string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	req := &btapb.DropRowRangeRequest{
		Name:   prefix + "/tables/" + table,
		Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true},
	}
	_, err := ac.tClient.DropRowRange(ctx, req)
	return err
}

// CreateTableFromSnapshot creates a table from snapshot.
// The table will be created in the same cluster as the snapshot.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) CreateTableFromSnapshot(ctx context.Context, table, cluster, snapshot string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	snapshotPath := prefix + "/clusters/" + cluster + "/snapshots/" + snapshot

	req := &btapb.CreateTableFromSnapshotRequest{
		Parent:         prefix,
		TableId:        table,
		SourceSnapshot: snapshotPath,
	}
	op, err := ac.tClient.CreateTableFromSnapshot(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Table{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// DefaultSnapshotDuration is the default TTL for a snapshot.
const DefaultSnapshotDuration time.Duration = 0

// SnapshotTable creates a new snapshot in the specified cluster from the
// specified source table. Setting the TTL to `DefaultSnapshotDuration` will
// use the server side default for the duration.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) SnapshotTable(ctx context.Context, table, cluster, snapshot string, ttl time.Duration) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()

	var ttlProto *durationpb.Duration

	if ttl > 0 {
		ttlProto = durationpb.New(ttl)
	}

	req := &btapb.SnapshotTableRequest{
		Name:       prefix + "/tables/" + table,
		Cluster:    prefix + "/clusters/" + cluster,
		SnapshotId: snapshot,
		Ttl:        ttlProto,
	}

	op, err := ac.tClient.SnapshotTable(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Snapshot{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// Snapshots returns a SnapshotIterator for iterating over the snapshots in a cluster.
// To list snapshots across all of the clusters in the instance specify "-" as the cluster.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature is not
// currently available to most Cloud Bigtable customers. This feature might be
// changed in backward-incompatible ways and is not recommended for production use.
// It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) Snapshots(ctx context.Context, cluster string) *SnapshotIterator {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster

	it := &SnapshotIterator{}
	req := &btapb.ListSnapshotsRequest{
		Parent: clusterPath,
	}

	fetch := func(pageSize int, pageToken string) (string, error) {
		req.PageToken = pageToken
		if pageSize > math.MaxInt32 {
			req.PageSize = math.MaxInt32
		} else {
			req.PageSize = int32(pageSize)
		}

		var resp *btapb.ListSnapshotsResponse
		err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
			var err error
			resp, err = ac.tClient.ListSnapshots(ctx, req)
			return err
		}, adminRetryOptions...)
		if err != nil {
			return "", err
		}
		for _, s := range resp.Snapshots {
			snapshotInfo, err := newSnapshotInfo(s)
			if err != nil {
				return "", fmt.Errorf("failed to parse snapshot proto %w", err)
			}
			it.items = append(it.items, snapshotInfo)
		}
		return resp.NextPageToken, nil
	}
	bufLen := func() int { return len(it.items) }
	takeBuf := func() interface{} { b := it.items; it.items = nil; return b }

	it.pageInfo, it.nextFunc = iterator.NewPageInfo(fetch, bufLen, takeBuf)

	return it
}

func newSnapshotInfo(snapshot *btapb.Snapshot) (*SnapshotInfo, error) {
	nameParts := strings.Split(snapshot.Name, "/")
	name := nameParts[len(nameParts)-1]
	tablePathParts := strings.Split(snapshot.SourceTable.Name, "/")
	tableID := tablePathParts[len(tablePathParts)-1]

	if err := snapshot.CreateTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid createTime: %w", err)
	}
	createTime := snapshot.GetCreateTime().AsTime()

	if err := snapshot.DeleteTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid deleteTime: %v", err)
	}
	deleteTime := snapshot.GetDeleteTime().AsTime()

	return &SnapshotInfo{
		Name:        name,
		SourceTable: tableID,
		DataSize:    snapshot.DataSizeBytes,
		CreateTime:  createTime,
		DeleteTime:  deleteTime,
	}, nil
}

// SnapshotIterator is an EntryIterator that iterates over log entries.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
type SnapshotIterator struct {
	items    []*SnapshotInfo
	pageInfo *iterator.PageInfo
	nextFunc func() error
}

// PageInfo supports pagination. See https://godoc.org/google.golang.org/api/iterator package for details.
func (it *SnapshotIterator) PageInfo() *iterator.PageInfo {
	return it.pageInfo
}

// Next returns the next result. Its second return value is iterator.Done
// (https://godoc.org/google.golang.org/api/iterator) if there are no more
// results. Once Next returns Done, all subsequent calls will return Done.
func (it *SnapshotIterator) Next() (*SnapshotInfo, error) {
	if err := it.nextFunc(); err != nil {
		return nil, err
	}
	item := it.items[0]
	it.items = it.items[1:]
	return item, nil
}

// SnapshotInfo contains snapshot metadata.
type SnapshotInfo struct {
	Name        string
	SourceTable string
	DataSize    int64
	CreateTime  time.Time
	DeleteTime  time.Time
}

// SnapshotInfo gets snapshot metadata.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) SnapshotInfo(ctx context.Context, cluster, snapshot string) (*SnapshotInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster
	snapshotPath := clusterPath + "/snapshots/" + snapshot

	req := &btapb.GetSnapshotRequest{
		Name: snapshotPath,
	}

	var resp *btapb.Snapshot
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		resp, err = ac.tClient.GetSnapshot(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	return newSnapshotInfo(resp)
}

// DeleteSnapshot deletes a snapshot in a cluster.
//
// This is a private alpha release of Cloud Bigtable snapshots. This feature
// is not currently available to most Cloud Bigtable customers. This feature
// might be changed in backward-incompatible ways and is not recommended for
// production use. It is not subject to any SLA or deprecation policy.
func (ac *AdminClient) DeleteSnapshot(ctx context.Context, cluster, snapshot string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster
	snapshotPath := clusterPath + "/snapshots/" + snapshot

	req := &btapb.DeleteSnapshotRequest{
		Name: snapshotPath,
	}
	_, err := ac.tClient.DeleteSnapshot(ctx, req)
	return err
}

// getConsistencyToken gets the consistency token for a table.
func (ac *AdminClient) getConsistencyToken(ctx context.Context, tableName string) (string, error) {
	req := &btapb.GenerateConsistencyTokenRequest{
		Name: tableName,
	}
	resp, err := ac.tClient.GenerateConsistencyToken(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.GetConsistencyToken(), nil
}

// isConsistent checks if a token is consistent for a table.
func (ac *AdminClient) isConsistent(ctx context.Context, tableName, token string) (bool, error) {
	req := &btapb.CheckConsistencyRequest{
		Name:             tableName,
		ConsistencyToken: token,
	}
	var resp *btapb.CheckConsistencyResponse

	// Retry calls on retryable errors to avoid losing the token gathered before.
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		resp, err = ac.tClient.CheckConsistency(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return false, err
	}
	return resp.GetConsistent(), nil
}

// WaitForReplication waits until all the writes committed before the call started have been propagated to all the clusters in the instance via replication.
func (ac *AdminClient) WaitForReplication(ctx context.Context, table string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	// Get the token.
	prefix := ac.instancePrefix()
	tableName := prefix + "/tables/" + table
	token, err := ac.getConsistencyToken(ctx, tableName)
	if err != nil {
		return err
	}

	// Periodically check if the token is consistent.
	timer := time.NewTicker(time.Second * 10)
	defer timer.Stop()
	for {
		consistent, err := ac.isConsistent(ctx, tableName, token)
		if err != nil {
			return err
		}
		if consistent {
			return nil
		}
		// Sleep for a bit or until the ctx is cancelled.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// TableIAM creates an IAM Handle specific to a given Instance and Table within the configured project.
func (ac *AdminClient) TableIAM(tableID string) *iam.Handle {
	return iam.InternalNewHandleGRPCClient(ac.tClient,
		"projects/"+ac.project+"/instances/"+ac.instance+"/tables/"+tableID)
}

// BackupIAM creates an IAM Handle specific to a given Cluster and Backup.
func (ac *AdminClient) BackupIAM(cluster, backup string) *iam.Handle {
	return iam.InternalNewHandleGRPCClient(ac.tClient, ac.backupPath(cluster, ac.instance, backup))
}

// AuthorizedViewIAM creates an IAM Handle specific to a given Table and AuthorizedView.
func (ac *AdminClient) AuthorizedViewIAM(table, authorizedView string) *iam.Handle {
	return iam.InternalNewHandleGRPCClient(ac.tClient, ac.authorizedViewPath(table, authorizedView))
}

// UNIVERSE_DOMAIN placeholder is replaced by the UniverseDomain from DialSettings while creating GRPC connection/dial pool.
const instanceAdminAddr = "bigtableadmin.UNIVERSE_DOMAIN:443"
const mtlsInstanceAdminAddr = "bigtableadmin.mtls.googleapis.com:443"

// InstanceAdminClient is a client type for performing admin operations on instances.
// These operations can be substantially more dangerous than those provided by AdminClient.
type InstanceAdminClient struct {
	connPool  gtransport.ConnPool
	iClient   btapb.BigtableInstanceAdminClient
	lroClient *lroauto.OperationsClient

	project string

	// Metadata to be sent with each request.
	md metadata.MD
}

// NewInstanceAdminClient creates a new InstanceAdminClient for a given project.
func NewInstanceAdminClient(ctx context.Context, project string, opts ...option.ClientOption) (*InstanceAdminClient, error) {
	o, err := btopt.DefaultClientOptions(instanceAdminAddr, mtlsInstanceAdminAddr, InstanceAdminScope, clientUserAgent)
	if err != nil {
		return nil, err
	}
	// Add gRPC client interceptors to supply Google client information. No external interceptors are passed.
	o = append(o, btopt.ClientInterceptorOptions(nil, nil)...)
	o = append(o, opts...)
	connPool, err := gtransport.DialPool(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	lroClient, err := lroauto.NewOperationsClient(ctx, gtransport.WithConnPool(connPool))
	if err != nil {
		// This error "should not happen", since we are just reusing old connection
		// and never actually need to dial.
		// If this does happen, we could leak conn. However, we cannot close conn:
		// If the user invoked the function with option.WithGRPCConn,
		// we would close a connection that's still in use.
		// TODO(pongad): investigate error conditions.
		return nil, err
	}

	return &InstanceAdminClient{
		connPool:  connPool,
		iClient:   btapb.NewBigtableInstanceAdminClient(connPool),
		lroClient: lroClient,

		project: project,
		md:      metadata.Pairs(resourcePrefixHeader, "projects/"+project),
	}, nil
}

// Close closes the InstanceAdminClient.
func (iac *InstanceAdminClient) Close() error {
	return iac.connPool.Close()
}

// StorageType is the type of storage used for all tables in an instance
type StorageType int

const (
	SSD StorageType = iota
	HDD
)

func (st StorageType) proto() btapb.StorageType {
	if st == HDD {
		return btapb.StorageType_HDD
	}
	return btapb.StorageType_SSD
}

func storageTypeFromProto(st btapb.StorageType) StorageType {
	if st == btapb.StorageType_HDD {
		return HDD
	}

	return SSD
}

// InstanceState is the state of the instance. This is output-only.
type InstanceState int32

const (
	// NotKnown represents the state of an instance that could not be determined.
	NotKnown InstanceState = InstanceState(btapb.Instance_STATE_NOT_KNOWN)
	// Ready represents the state of an instance that has been successfully created.
	Ready = InstanceState(btapb.Instance_READY)
	// Creating represents the state of an instance that is currently being created.
	Creating = InstanceState(btapb.Instance_CREATING)
)

// InstanceType is the type of the instance.
type InstanceType int32

const (
	// UNSPECIFIED instance types default to PRODUCTION
	UNSPECIFIED InstanceType = InstanceType(btapb.Instance_TYPE_UNSPECIFIED)
	PRODUCTION               = InstanceType(btapb.Instance_PRODUCTION)
	DEVELOPMENT              = InstanceType(btapb.Instance_DEVELOPMENT)
)

// InstanceInfo represents information about an instance
type InstanceInfo struct {
	Name          string // name of the instance
	DisplayName   string // display name for UIs
	InstanceState InstanceState
	InstanceType  InstanceType
	Labels        map[string]string
}

// InstanceConf contains the information necessary to create an Instance
type InstanceConf struct {
	InstanceId, DisplayName, ClusterId, Zone string
	// NumNodes must not be specified for DEVELOPMENT instance types
	NumNodes     int32
	StorageType  StorageType
	InstanceType InstanceType
	Labels       map[string]string

	// AutoscalingConfig configures the autoscaling properties on the cluster
	// created with the instance. It is optional.
	AutoscalingConfig *AutoscalingConfig

	// NodeScalingFactor controls the scaling factor of the cluster (i.e. the
	// increment in which NumNodes can be set). Node scaling delivers better
	// latency and more throughput by removing node boundaries. It is optional,
	// with the default being 1X.
	NodeScalingFactor NodeScalingFactor
}

// InstanceWithClustersConfig contains the information necessary to create an Instance
type InstanceWithClustersConfig struct {
	InstanceID, DisplayName string
	Clusters                []ClusterConfig
	InstanceType            InstanceType
	Labels                  map[string]string
}

var instanceNameRegexp = regexp.MustCompile(`^projects/([^/]+)/instances/([a-z][-a-z0-9]*)$`)

// CreateInstance creates a new instance in the project.
// This method will return when the instance has been created or when an error occurs.
func (iac *InstanceAdminClient) CreateInstance(ctx context.Context, conf *InstanceConf) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	newConfig := &InstanceWithClustersConfig{
		InstanceID:   conf.InstanceId,
		DisplayName:  conf.DisplayName,
		InstanceType: conf.InstanceType,
		Labels:       conf.Labels,
		Clusters: []ClusterConfig{
			{
				InstanceID:        conf.InstanceId,
				ClusterID:         conf.ClusterId,
				Zone:              conf.Zone,
				NumNodes:          conf.NumNodes,
				StorageType:       conf.StorageType,
				AutoscalingConfig: conf.AutoscalingConfig,
				NodeScalingFactor: conf.NodeScalingFactor,
			},
		},
	}
	return iac.CreateInstanceWithClusters(ctx, newConfig)
}

// CreateInstanceWithClusters creates a new instance with configured clusters in the project.
// This method will return when the instance has been created or when an error occurs.
func (iac *InstanceAdminClient) CreateInstanceWithClusters(ctx context.Context, conf *InstanceWithClustersConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	clusters := make(map[string]*btapb.Cluster)
	for _, cluster := range conf.Clusters {
		clusters[cluster.ClusterID] = cluster.proto(iac.project)
	}

	req := &btapb.CreateInstanceRequest{
		Parent:     "projects/" + iac.project,
		InstanceId: conf.InstanceID,
		Instance: &btapb.Instance{
			DisplayName: conf.DisplayName,
			Type:        btapb.Instance_Type(conf.InstanceType),
			Labels:      conf.Labels,
		},
		Clusters: clusters,
	}

	lro, err := iac.iClient.CreateInstance(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Instance{}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, &resp)
}

// updateInstance updates a single instance based on config fields that operate
// at an instance level: DisplayName and InstanceType.
func (iac *InstanceAdminClient) updateInstance(ctx context.Context, conf *InstanceWithClustersConfig) (updated bool, err error) {
	if conf.InstanceID == "" {
		return false, errors.New("InstanceID is required")
	}

	// Update the instance, if necessary
	mask := &field_mask.FieldMask{}
	ireq := &btapb.PartialUpdateInstanceRequest{
		Instance: &btapb.Instance{
			Name: "projects/" + iac.project + "/instances/" + conf.InstanceID,
		},
		UpdateMask: mask,
	}
	if conf.DisplayName != "" {
		ireq.Instance.DisplayName = conf.DisplayName
		mask.Paths = append(mask.Paths, "display_name")
	}
	if btapb.Instance_Type(conf.InstanceType) != btapb.Instance_TYPE_UNSPECIFIED {
		ireq.Instance.Type = btapb.Instance_Type(conf.InstanceType)
		mask.Paths = append(mask.Paths, "type")
	}
	if conf.Labels != nil {
		ireq.Instance.Labels = conf.Labels
		mask.Paths = append(mask.Paths, "labels")
	}

	if len(mask.Paths) == 0 {
		return false, nil
	}

	lro, err := iac.iClient.PartialUpdateInstance(ctx, ireq)
	if err != nil {
		return false, err
	}
	err = longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, nil)
	if err != nil {
		return false, err
	}

	return true, nil
}

// UpdateInstanceWithClusters updates an instance and its clusters. Updateable
// fields are instance display name, instance type and cluster size.
// The provided InstanceWithClustersConfig is used as follows:
//   - InstanceID is required
//   - DisplayName and InstanceType are updated only if they are not empty
//   - ClusterID is required for any provided cluster
//   - All other cluster fields are ignored except for NumNodes and
//     AutoscalingConfig, which if set will be updated. If both are provided,
//     AutoscalingConfig takes precedence.
//
// This method may return an error after partially succeeding, for example if the instance is updated
// but a cluster update fails. If an error is returned, InstanceInfo and Clusters may be called to
// determine the current state.
func (iac *InstanceAdminClient) UpdateInstanceWithClusters(ctx context.Context, conf *InstanceWithClustersConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)

	for _, cluster := range conf.Clusters {
		if cluster.ClusterID == "" {
			return errors.New("ClusterID is required for every cluster")
		}
	}

	updatedInstance, err := iac.updateInstance(ctx, conf)
	if err != nil {
		return err
	}

	// Update any clusters
	for _, cluster := range conf.Clusters {
		var clusterErr error
		if cluster.AutoscalingConfig != nil {
			clusterErr = iac.SetAutoscaling(ctx, conf.InstanceID, cluster.ClusterID, *cluster.AutoscalingConfig)
		} else if cluster.NumNodes > 0 {
			clusterErr = iac.UpdateCluster(ctx, conf.InstanceID, cluster.ClusterID, cluster.NumNodes)
		}
		if clusterErr != nil {
			if updatedInstance {
				// We updated the instance, so note that in the error message.
				return fmt.Errorf("UpdateCluster %q failed %w; however UpdateInstance succeeded",
					cluster.ClusterID, clusterErr)
			}
			return clusterErr
		}
	}

	return nil
}

// DeleteInstance deletes an instance from the project.
func (iac *InstanceAdminClient) DeleteInstance(ctx context.Context, instanceID string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteInstanceRequest{Name: "projects/" + iac.project + "/instances/" + instanceID}
	_, err := iac.iClient.DeleteInstance(ctx, req)
	return err
}

// Instances returns a list of instances in the project. If any location
// (cluster) is unavailable due to some transient conditions, Instances
// returns partial results and ErrPartiallyUnavailable error with
// unavailable locations list.
func (iac *InstanceAdminClient) Instances(ctx context.Context) ([]*InstanceInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.ListInstancesRequest{
		Parent: "projects/" + iac.project,
	}
	var res *btapb.ListInstancesResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.ListInstances(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	var is []*InstanceInfo
	for _, i := range res.Instances {
		m := instanceNameRegexp.FindStringSubmatch(i.Name)
		if m == nil {
			return nil, fmt.Errorf("malformed instance name %q", i.Name)
		}
		is = append(is, &InstanceInfo{
			Name:          m[2],
			DisplayName:   i.DisplayName,
			InstanceState: InstanceState(i.State),
			InstanceType:  InstanceType(i.Type),
			Labels:        i.Labels,
		})
	}
	if len(res.FailedLocations) > 0 {
		// Return partial results and an error in
		// case of some locations are unavailable.
		return is, ErrPartiallyUnavailable{res.FailedLocations}
	}
	return is, nil
}

// InstanceInfo returns information about an instance.
func (iac *InstanceAdminClient) InstanceInfo(ctx context.Context, instanceID string) (*InstanceInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.GetInstanceRequest{
		Name: "projects/" + iac.project + "/instances/" + instanceID,
	}
	var res *btapb.Instance
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.GetInstance(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	m := instanceNameRegexp.FindStringSubmatch(res.Name)
	if m == nil {
		return nil, fmt.Errorf("malformed instance name %q", res.Name)
	}
	return &InstanceInfo{
		Name:          m[2],
		DisplayName:   res.DisplayName,
		InstanceState: InstanceState(res.State),
		InstanceType:  InstanceType(res.Type),
		Labels:        res.Labels,
	}, nil
}

// AutoscalingConfig contains autoscaling configuration for a cluster.
// For details, see https://cloud.google.com/bigtable/docs/autoscaling.
type AutoscalingConfig struct {
	// MinNodes sets the minimum number of nodes in a cluster. MinNodes must
	// be 1 or greater.
	MinNodes int
	// MaxNodes sets the maximum number of nodes in a cluster. MaxNodes must be
	// equal to or greater than MinNodes.
	MaxNodes int
	// CPUTargetPercent sets the CPU utilization target for your cluster's
	// workload.
	CPUTargetPercent int
	// StorageUtilizationPerNode sets the storage usage target, in GB, for
	// each node in a cluster. This number is limited between 2560 (2.5TiB) and
	// 5120 (5TiB) for a SSD cluster and between 8192 (8TiB) and 16384 (16 TiB)
	// for an HDD cluster. If set to zero, the default values are used:
	// 2560 for SSD and 8192 for HDD.
	StorageUtilizationPerNode int
}

func (a *AutoscalingConfig) proto() *btapb.Cluster_ClusterAutoscalingConfig {
	if a == nil {
		return nil
	}
	return &btapb.Cluster_ClusterAutoscalingConfig{
		AutoscalingLimits: &btapb.AutoscalingLimits{
			MinServeNodes: int32(a.MinNodes),
			MaxServeNodes: int32(a.MaxNodes),
		},
		AutoscalingTargets: &btapb.AutoscalingTargets{
			CpuUtilizationPercent:        int32(a.CPUTargetPercent),
			StorageUtilizationGibPerNode: int32(a.StorageUtilizationPerNode),
		},
	}
}

// NodeScalingFactor controls the scaling factor of the cluster (i.e. the
// increment in which NumNodes can be set). Node scaling delivers better
// latency and more throughput by removing node boundaries.
type NodeScalingFactor int32

const (
	// NodeScalingFactorUnspecified default to 1X.
	NodeScalingFactorUnspecified NodeScalingFactor = iota
	// NodeScalingFactor1X runs the cluster with a scaling factor of 1.
	NodeScalingFactor1X
	// NodeScalingFactor2X runs the cluster with a scaling factor of 2.
	// All node count values must be in increments of 2 with this scaling
	// factor enabled, otherwise an INVALID_ARGUMENT error will be returned.
	NodeScalingFactor2X
)

func (nsf NodeScalingFactor) proto() btapb.Cluster_NodeScalingFactor {
	switch nsf {
	case NodeScalingFactor1X:
		return btapb.Cluster_NODE_SCALING_FACTOR_1X
	case NodeScalingFactor2X:
		return btapb.Cluster_NODE_SCALING_FACTOR_2X
	default:
		return btapb.Cluster_NODE_SCALING_FACTOR_UNSPECIFIED
	}
}

func nodeScalingFactorFromProto(nsf btapb.Cluster_NodeScalingFactor) NodeScalingFactor {
	switch nsf {
	case btapb.Cluster_NODE_SCALING_FACTOR_1X:
		return NodeScalingFactor1X
	case btapb.Cluster_NODE_SCALING_FACTOR_2X:
		return NodeScalingFactor2X
	default:
		return NodeScalingFactorUnspecified
	}
}

// ClusterConfig contains the information necessary to create a cluster
type ClusterConfig struct {
	// InstanceID specifies the unique name of the instance. Required.
	InstanceID string

	// ClusterID specifies the unique name of the cluster. Required.
	ClusterID string

	// Zone specifies the location where this cluster's nodes and storage reside.
	// For best performance, clients should be located as close as possible to this
	// cluster. Required.
	Zone string

	// NumNodes specifies the number of nodes allocated to this cluster. More
	// nodes enable higher throughput and more consistent performance. One of
	// NumNodes or AutoscalingConfig is required. If both are set,
	// AutoscalingConfig takes precedence.
	NumNodes int32

	// StorageType specifies the type of storage used by this cluster to serve
	// its parent instance's tables, unless explicitly overridden. Required.
	StorageType StorageType

	// KMSKeyName is the name of the KMS customer managed encryption key (CMEK)
	// to use for at-rest encryption of data in this cluster.  If omitted,
	// Google's default encryption will be used. If specified, the requirements
	// for this key are:
	// 1) The Cloud Bigtable service account associated with the
	//    project that contains the cluster must be granted the
	//    ``cloudkms.cryptoKeyEncrypterDecrypter`` role on the
	//    CMEK.
	// 2) Only regional keys can be used and the region of the
	//    CMEK key must match the region of the cluster.
	// 3) All clusters within an instance must use the same CMEK
	//    key.
	// Optional. Immutable.
	KMSKeyName string

	// AutoscalingConfig configures the autoscaling properties on a cluster.
	// One of NumNodes or AutoscalingConfig is required.
	AutoscalingConfig *AutoscalingConfig

	// NodeScalingFactor controls the scaling factor of the cluster (i.e. the
	// increment in which NumNodes can be set). Node scaling delivers better
	// latency and more throughput by removing node boundaries. It is optional,
	// with the default being 1X.
	NodeScalingFactor NodeScalingFactor
}

func (cc *ClusterConfig) proto(project string) *btapb.Cluster {
	cl := &btapb.Cluster{
		ServeNodes:         cc.NumNodes,
		DefaultStorageType: cc.StorageType.proto(),
		Location:           "projects/" + project + "/locations/" + cc.Zone,
		EncryptionConfig: &btapb.Cluster_EncryptionConfig{
			KmsKeyName: cc.KMSKeyName,
		},
		NodeScalingFactor: cc.NodeScalingFactor.proto(),
	}

	if asc := cc.AutoscalingConfig; asc != nil {
		cl.Config = &btapb.Cluster_ClusterConfig_{
			ClusterConfig: &btapb.Cluster_ClusterConfig{
				ClusterAutoscalingConfig: asc.proto(),
			},
		}
	}
	return cl
}

// ClusterInfo represents information about a cluster.
type ClusterInfo struct {
	// Name is the name of the cluster.
	Name string

	// Zone is the GCP zone of the cluster (e.g. "us-central1-a").
	Zone string

	// ServeNodes is the number of allocated serve nodes.
	ServeNodes int

	// State is the state of the cluster.
	State string

	// StorageType is the storage type of the cluster.
	StorageType StorageType

	// KMSKeyName is the customer managed encryption key for the cluster.
	KMSKeyName string

	// AutoscalingConfig are the configured values for a cluster.
	AutoscalingConfig *AutoscalingConfig

	// NodeScalingFactor controls the scaling factor of the cluster.
	NodeScalingFactor NodeScalingFactor
}

// CreateCluster creates a new cluster in an instance.
// This method will return when the cluster has been created or when an error occurs.
func (iac *InstanceAdminClient) CreateCluster(ctx context.Context, conf *ClusterConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)

	req := &btapb.CreateClusterRequest{
		Parent:    "projects/" + iac.project + "/instances/" + conf.InstanceID,
		ClusterId: conf.ClusterID,
		Cluster:   conf.proto(iac.project),
	}

	lro, err := iac.iClient.CreateCluster(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Cluster{}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, &resp)
}

// DeleteCluster deletes a cluster from an instance.
func (iac *InstanceAdminClient) DeleteCluster(ctx context.Context, instanceID, clusterID string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteClusterRequest{Name: "projects/" + iac.project + "/instances/" + instanceID + "/clusters/" + clusterID}
	_, err := iac.iClient.DeleteCluster(ctx, req)
	return err
}

// SetAutoscaling enables autoscaling on a cluster. To remove autoscaling, use
// UpdateCluster. See AutoscalingConfig documentation for details.
func (iac *InstanceAdminClient) SetAutoscaling(ctx context.Context, instanceID, clusterID string, conf AutoscalingConfig) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	cluster := &btapb.Cluster{
		Name: "projects/" + iac.project + "/instances/" + instanceID + "/clusters/" + clusterID,
		Config: &btapb.Cluster_ClusterConfig_{
			ClusterConfig: &btapb.Cluster_ClusterConfig{
				ClusterAutoscalingConfig: conf.proto(),
			},
		},
	}
	lro, err := iac.iClient.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"cluster_config.cluster_autoscaling_config"},
		},
		Cluster: cluster,
	})
	if err != nil {
		return err
	}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, nil)
}

// UpdateCluster updates attributes of a cluster. If Autoscaling is configured
// for the cluster, it will be removed and replaced by the static number of
// serve nodes specified.
func (iac *InstanceAdminClient) UpdateCluster(ctx context.Context, instanceID, clusterID string, serveNodes int32) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	cluster := &btapb.Cluster{
		Name:       "projects/" + iac.project + "/instances/" + instanceID + "/clusters/" + clusterID,
		ServeNodes: serveNodes,
		// Explicitly removing autoscaling config (and including it in the field
		// mask below)
		Config: nil,
	}
	lro, err := iac.iClient.PartialUpdateCluster(ctx, &btapb.PartialUpdateClusterRequest{
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"serve_nodes", "cluster_config.cluster_autoscaling_config"},
		},
		Cluster: cluster,
	})
	if err != nil {
		return err
	}
	return longrunning.InternalNewOperation(iac.lroClient, lro).Wait(ctx, nil)
}

// Clusters lists the clusters in an instance. If any location
// (cluster) is unavailable due to some transient conditions, Clusters
// returns partial results and ErrPartiallyUnavailable error with
// unavailable locations list.
func (iac *InstanceAdminClient) Clusters(ctx context.Context, instanceID string) ([]*ClusterInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.ListClustersRequest{Parent: "projects/" + iac.project + "/instances/" + instanceID}
	var res *btapb.ListClustersResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.ListClusters(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	var cis []*ClusterInfo
	for _, c := range res.Clusters {
		nameParts := strings.Split(c.Name, "/")
		locParts := strings.Split(c.Location, "/")
		kmsKeyName := ""
		if c.EncryptionConfig != nil {
			kmsKeyName = c.EncryptionConfig.KmsKeyName
		}
		ci := &ClusterInfo{
			Name:              nameParts[len(nameParts)-1],
			Zone:              locParts[len(locParts)-1],
			ServeNodes:        int(c.ServeNodes),
			State:             c.State.String(),
			StorageType:       storageTypeFromProto(c.DefaultStorageType),
			KMSKeyName:        kmsKeyName,
			NodeScalingFactor: nodeScalingFactorFromProto(c.NodeScalingFactor),
		}
		if cfg := c.GetClusterConfig(); cfg != nil {
			if asc := fromClusterConfigProto(cfg); asc != nil {
				ci.AutoscalingConfig = asc
			}
		}
		cis = append(cis, ci)
	}
	if len(res.FailedLocations) > 0 {
		// Return partial results and an error in
		// case of some locations are unavailable.
		return cis, ErrPartiallyUnavailable{res.FailedLocations}
	}
	return cis, nil
}

// GetCluster fetches a cluster in an instance
func (iac *InstanceAdminClient) GetCluster(ctx context.Context, instanceID, clusterID string) (*ClusterInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.GetClusterRequest{
		Name: fmt.Sprintf("projects/%s/instances/%s/clusters/%s", iac.project, instanceID, clusterID),
	}
	var c *btapb.Cluster
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		c, err = iac.iClient.GetCluster(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	kmsKeyName := ""
	if c.EncryptionConfig != nil {
		kmsKeyName = c.EncryptionConfig.KmsKeyName
	}
	nameParts := strings.Split(c.Name, "/")
	locParts := strings.Split(c.Location, "/")
	ci := &ClusterInfo{
		Name:              nameParts[len(nameParts)-1],
		Zone:              locParts[len(locParts)-1],
		ServeNodes:        int(c.ServeNodes),
		State:             c.State.String(),
		StorageType:       storageTypeFromProto(c.DefaultStorageType),
		KMSKeyName:        kmsKeyName,
		NodeScalingFactor: nodeScalingFactorFromProto(c.NodeScalingFactor),
	}
	// Use type assertion to handle protobuf oneof type
	if cfg := c.GetClusterConfig(); cfg != nil {
		if asc := fromClusterConfigProto(cfg); asc != nil {
			ci.AutoscalingConfig = asc
		}
	}
	return ci, nil
}

func fromClusterConfigProto(c *btapb.Cluster_ClusterConfig) *AutoscalingConfig {
	if c == nil {
		return nil
	}
	if c.ClusterAutoscalingConfig == nil {
		return nil
	}
	got := c.ClusterAutoscalingConfig
	if got.AutoscalingLimits == nil || got.AutoscalingTargets == nil {
		return nil
	}
	return &AutoscalingConfig{
		MinNodes:                  int(got.AutoscalingLimits.MinServeNodes),
		MaxNodes:                  int(got.AutoscalingLimits.MaxServeNodes),
		CPUTargetPercent:          int(got.AutoscalingTargets.CpuUtilizationPercent),
		StorageUtilizationPerNode: int(got.AutoscalingTargets.StorageUtilizationGibPerNode),
	}
}

// InstanceIAM returns the instance's IAM handle.
func (iac *InstanceAdminClient) InstanceIAM(instanceID string) *iam.Handle {
	return iam.InternalNewHandleGRPCClient(iac.iClient, "projects/"+iac.project+"/instances/"+instanceID)
}

// Routing policies.
const (
	// Deprecated: Use MultiClusterRoutingUseAnyConfig instead.
	// MultiClusterRouting is a policy that allows read/write requests to be
	// routed to any cluster in the instance. Requests will will fail over to
	// another cluster in the event of transient errors or delays. Choosing
	// this option sacrifices read-your-writes consistency to improve
	// availability.
	MultiClusterRouting = "multi_cluster_routing_use_any"
	// Deprecated: Use SingleClusterRoutingConfig instead.
	// SingleClusterRouting is a policy that unconditionally routes all
	// read/write requests to a specific cluster. This option preserves
	// read-your-writes consistency, but does not improve availability.
	SingleClusterRouting = "single_cluster_routing"
)

// ProfileConf contains the information necessary to create a profile
type ProfileConf struct {
	Name        string
	ProfileID   string
	InstanceID  string
	Etag        string
	Description string

	RoutingConfig RoutingPolicyConfig
	Isolation     AppProfileIsolation

	// Deprecated: Use RoutingConfig instead.
	// Ignored when RoutingConfig is set.
	RoutingPolicy string
	// Deprecated: Use RoutingConfig with SingleClusterRoutingConfig instead.
	// Ignored when RoutingConfig is set.
	// To use with RoutingPolicy field while specifying SingleClusterRouting.
	ClusterID string
	// Deprecated: Use RoutingConfig with SingleClusterRoutingConfig instead.
	// Ignored when RoutingConfig is set.
	// To use with RoutingPolicy field while specifying SingleClusterRouting.
	AllowTransactionalWrites bool

	// If true, warnings are ignored
	IgnoreWarnings bool
}

func setIsolation(profile *btapb.AppProfile, isolation AppProfileIsolation) error {
	if isolation != nil {
		switch cfg := isolation.(type) {
		case *StandardIsolation:
			profile.Isolation = &btapb.AppProfile_StandardIsolation_{
				StandardIsolation: &btapb.AppProfile_StandardIsolation{
					Priority: btapb.AppProfile_Priority(cfg.Priority),
				},
			}
		case *DataBoostIsolationReadOnly:
			dataBoostProto := &btapb.AppProfile_DataBoostIsolationReadOnly{}
			cbo := btapb.AppProfile_DataBoostIsolationReadOnly_ComputeBillingOwner(cfg.ComputeBillingOwner)
			dataBoostProto.ComputeBillingOwner = &cbo
			profile.Isolation = &btapb.AppProfile_DataBoostIsolationReadOnly_{DataBoostIsolationReadOnly: dataBoostProto}
		default:
			return fmt.Errorf("bigtable: unknown isolation config type: %T", cfg)
		}
	}
	return nil
}

func setRoutingPolicy(appProfile *btapb.AppProfile, rpc RoutingPolicyConfig, routingPolicy optional.String,
	clusterID string, allowTransactionalWrites bool, allowNil bool) error {
	if allowNil && routingPolicy == nil && rpc == nil {
		return nil
	}
	if rpc != nil {
		switch cfg := rpc.(type) {
		case *MultiClusterRoutingUseAnyConfig:
			appProfile.RoutingPolicy = &btapb.AppProfile_MultiClusterRoutingUseAny_{
				MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{
					ClusterIds: cfg.ClusterIDs,
				},
			}
			if cfg.Affinity != nil {
				switch cfg.Affinity.(type) {
				case *RowAffinity:
					appProfile.GetMultiClusterRoutingUseAny().Affinity = &btapb.AppProfile_MultiClusterRoutingUseAny_RowAffinity_{
						RowAffinity: &btapb.AppProfile_MultiClusterRoutingUseAny_RowAffinity{},
					}
				default:
					return errors.New("bigtable: invalid affinity in MultiClusterRoutingUseAnyConfig")
				}
			}
		case *SingleClusterRoutingConfig:
			appProfile.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{
				SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
					ClusterId:                cfg.ClusterID,
					AllowTransactionalWrites: cfg.AllowTransactionalWrites,
				},
			}
		default:
			return fmt.Errorf("bigtable: unknown RoutingConfig type: %T", cfg)
		}
	} else { // Fallback to deprecated fields
		if routingPolicy == nil {
			return errors.New("bigtable: at least one of RoutingPolicy or RoutingConfig must be set")
		}

		switch routingPolicy {
		case MultiClusterRouting:
			appProfile.RoutingPolicy = &btapb.AppProfile_MultiClusterRoutingUseAny_{
				MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{},
			}
		case SingleClusterRouting:
			appProfile.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{
				SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
					ClusterId:                clusterID,
					AllowTransactionalWrites: allowTransactionalWrites,
				},
			}
		default:
			return errors.New("bigtable: invalid RoutingPolicy " + optional.ToString(routingPolicy))
		}
	}
	return nil
}

// ProfileIterator iterates over profiles.
type ProfileIterator struct {
	items    []*btapb.AppProfile
	pageInfo *iterator.PageInfo
	nextFunc func() error
}

// ProfileAttrsToUpdate define addrs to update during an Update call. If unset, no fields will be replaced.
type ProfileAttrsToUpdate struct {
	// If set, updates the description.
	Description optional.String

	// If set, updates the routing policy.
	// Takes precedence over deprecated RoutingPolicy, ClusterID and AllowTransactionalWrites.
	RoutingConfig RoutingPolicyConfig

	// If set, updates the isolation options.
	Isolation AppProfileIsolation

	// If set, updates the routing policy.
	// Deprecated: Use RoutingConfig instead.
	RoutingPolicy optional.String

	// If RoutingPolicy is updated to SingleClusterRouting, set this field as well.
	// Deprecated: Use RoutingConfig with SingleClusterRoutingConfig instead
	ClusterID string
	// If RoutingPolicy is updated to SingleClusterRouting, set this field as well.
	// Deprecated: Use RoutingConfig with SingleClusterRoutingConfig instead
	AllowTransactionalWrites bool

	// If true, warnings are ignored
	IgnoreWarnings bool
}

// GetFieldMaskPath returns the field mask path.
func (p *ProfileAttrsToUpdate) GetFieldMaskPath() []string {
	path := make([]string, 0)
	if p.Description != nil {
		path = append(path, "description")
	}

	if p.RoutingConfig != nil {
		path = append(path, p.RoutingConfig.getFieldMaskPath())
	} else if p.RoutingPolicy != nil {
		path = append(path, optional.ToString(p.RoutingPolicy))
	}
	if p.Isolation != nil {
		path = append(path, p.Isolation.getFieldMaskPath())
	}

	return path
}

// RoutingPolicyConfig represents the configuration for a specific routing policy.
type RoutingPolicyConfig interface {
	isRoutingPolicyConfig()
	getFieldMaskPath() string
}

// SingleClusterRoutingConfig is a policy that unconditionally routes all
// read/write requests to a specific cluster. This option preserves
// read-your-writes consistency, but does not improve availability.
type SingleClusterRoutingConfig struct {
	// The cluster to which read/write requests should be routed.
	ClusterID string
	// Whether or not `CheckAndMutateRow` and `ReadModifyWriteRow` requests are
	// allowed by this app profile. It is unsafe to send these requests to
	// the same table/row/column in multiple clusters.
	AllowTransactionalWrites bool
}

func (*SingleClusterRoutingConfig) isRoutingPolicyConfig()   {}
func (*SingleClusterRoutingConfig) getFieldMaskPath() string { return "single_cluster_routing" }

// MultiClusterRoutingUseAnyConfig is a policy whererin read/write requests are
// routed to the nearest cluster in the instance, and
// will fail over to the nearest cluster that is available in the event of
// transient errors or delays. Clusters in a region are considered
// equidistant. Choosing this option sacrifices read-your-writes consistency
// to improve availability.
type MultiClusterRoutingUseAnyConfig struct {
	// The set of clusters to route to. The order is ignored; clusters will be
	// tried in order of distance. If left empty, all clusters are eligible.
	ClusterIDs []string

	// Possible algorithms for routing affinity. If enabled, Bigtable will
	// route between equidistant clusters in a deterministic order rather than
	// choosing randomly.
	Affinity MultiClusterRoutingUseAnyAffinity
}

func (*MultiClusterRoutingUseAnyConfig) isRoutingPolicyConfig() {}
func (*MultiClusterRoutingUseAnyConfig) getFieldMaskPath() string {
	return "multi_cluster_routing_use_any"
}

// MultiClusterRoutingUseAnyAffinity represents the configuration for a specific affinity strategy.
type MultiClusterRoutingUseAnyAffinity interface {
	isMultiClusterRoutingUseAnyAffinity()
}

// RowAffinity enables row-based affinity.
// If enabled, Bigtable will route the request based on the row key of the
// request, rather than randomly. Instead, each row key will be assigned
// to a cluster, and will stick to that cluster.
type RowAffinity struct{}

func (*RowAffinity) isMultiClusterRoutingUseAnyAffinity() {}

// AppProfileIsolation represents the configuration for a specific traffic isolation policy.
type AppProfileIsolation interface {
	isAppProfileIsolation()
	getFieldMaskPath() string
}

// StandardIsolation configures standard traffic isolation.
type StandardIsolation struct {
	Priority AppProfilePriority
}

func (*StandardIsolation) isAppProfileIsolation()   {}
func (*StandardIsolation) getFieldMaskPath() string { return "standard_isolation" }

// AppProfilePriority represents possible priorities for an app profile.
type AppProfilePriority int32

const (
	// AppProfilePriorityUnspecified is the default value. Mapped to PRIORITY_HIGH (the legacy behavior) on creation.
	AppProfilePriorityUnspecified AppProfilePriority = AppProfilePriority(btapb.AppProfile_PRIORITY_UNSPECIFIED)
	// AppProfilePriorityLow represents the lowest priority.
	AppProfilePriorityLow AppProfilePriority = AppProfilePriority(btapb.AppProfile_PRIORITY_LOW)
	// AppProfilePriorityMedium represents the medium priority.
	AppProfilePriorityMedium AppProfilePriority = AppProfilePriority(btapb.AppProfile_PRIORITY_MEDIUM)
	// AppProfilePriorityHigh represents the highest priority.
	AppProfilePriorityHigh AppProfilePriority = AppProfilePriority(btapb.AppProfile_PRIORITY_HIGH)
)

// DataBoostIsolationReadOnly configures Data Boost isolation.
// Data Boost is a serverless compute capability that lets you run
// high-throughput read jobs and queries on your Bigtable data, without
// impacting the performance of the clusters that handle your application
// traffic. Data Boost supports read-only use cases with single-cluster
// routing.
type DataBoostIsolationReadOnly struct {
	// Compute Billing Owner specifies how usage should be accounted when using
	// Data Boost. Compute Billing Owner also configures which Cloud Project is
	// charged for relevant quota.
	ComputeBillingOwner IsolationComputeBillingOwner
}

func (*DataBoostIsolationReadOnly) isAppProfileIsolation()   {}
func (*DataBoostIsolationReadOnly) getFieldMaskPath() string { return "data_boost_isolation_read_only" }

// IsolationComputeBillingOwner specifies how usage should be accounted when using
// Data Boost. Compute Billing Owner also configures which Cloud Project is
// charged for relevant quota.
type IsolationComputeBillingOwner int32

const (
	// ComputeBillingOwnerUnspecified is the default value.
	ComputeBillingOwnerUnspecified IsolationComputeBillingOwner = IsolationComputeBillingOwner(btapb.AppProfile_DataBoostIsolationReadOnly_COMPUTE_BILLING_OWNER_UNSPECIFIED)
	// HostPays indicates that the host Cloud Project containing the targeted Bigtable Instance /
	// Table pays for compute.
	HostPays IsolationComputeBillingOwner = IsolationComputeBillingOwner(btapb.AppProfile_DataBoostIsolationReadOnly_HOST_PAYS)
)

// PageInfo supports pagination. See https://godoc.org/google.golang.org/api/iterator package for details.
func (it *ProfileIterator) PageInfo() *iterator.PageInfo {
	return it.pageInfo
}

// Next returns the next result. Its second return value is iterator.Done
// (https://godoc.org/google.golang.org/api/iterator) if there are no more
// results. Once Next returns Done, all subsequent calls will return Done.
func (it *ProfileIterator) Next() (*btapb.AppProfile, error) {
	if err := it.nextFunc(); err != nil {
		return nil, err
	}
	item := it.items[0]
	it.items = it.items[1:]
	return item, nil
}

// CreateAppProfile creates an app profile within an instance.
func (iac *InstanceAdminClient) CreateAppProfile(ctx context.Context, profile ProfileConf) (*btapb.AppProfile, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	parent := "projects/" + iac.project + "/instances/" + profile.InstanceID
	appProfile := &btapb.AppProfile{
		Etag:        profile.Etag,
		Description: profile.Description,
	}

	err := setRoutingPolicy(appProfile, profile.RoutingConfig, optional.String(profile.RoutingPolicy), profile.ClusterID, profile.AllowTransactionalWrites, false)
	if err != nil {
		return nil, err
	}

	err = setIsolation(appProfile, profile.Isolation)
	if err != nil {
		return nil, err
	}

	return iac.iClient.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{
		Parent:         parent,
		AppProfile:     appProfile,
		AppProfileId:   profile.ProfileID,
		IgnoreWarnings: profile.IgnoreWarnings,
	})
}

// GetAppProfile gets information about an app profile.
func (iac *InstanceAdminClient) GetAppProfile(ctx context.Context, instanceID, name string) (*btapb.AppProfile, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	profileRequest := &btapb.GetAppProfileRequest{
		Name: "projects/" + iac.project + "/instances/" + instanceID + "/appProfiles/" + name,
	}
	var ap *btapb.AppProfile
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		ap, err = iac.iClient.GetAppProfile(ctx, profileRequest)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}
	return ap, err
}

// ListAppProfiles lists information about app profiles in an instance.
func (iac *InstanceAdminClient) ListAppProfiles(ctx context.Context, instanceID string) *ProfileIterator {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	listRequest := &btapb.ListAppProfilesRequest{
		Parent: "projects/" + iac.project + "/instances/" + instanceID,
	}

	pit := &ProfileIterator{}
	fetch := func(pageSize int, pageToken string) (string, error) {
		listRequest.PageToken = pageToken
		var profileRes *btapb.ListAppProfilesResponse
		err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
			var err error
			profileRes, err = iac.iClient.ListAppProfiles(ctx, listRequest)
			return err
		}, adminRetryOptions...)
		if err != nil {
			return "", err
		}

		pit.items = append(pit.items, profileRes.AppProfiles...)
		return profileRes.NextPageToken, nil
	}

	bufLen := func() int { return len(pit.items) }
	takeBuf := func() interface{} { b := pit.items; pit.items = nil; return b }
	pit.pageInfo, pit.nextFunc = iterator.NewPageInfo(fetch, bufLen, takeBuf)
	return pit

}

// UpdateAppProfile updates an app profile within an instance.
// updateAttrs should be set. If unset, all fields will be replaced.
func (iac *InstanceAdminClient) UpdateAppProfile(ctx context.Context, instanceID, profileID string, updateAttrs ProfileAttrsToUpdate) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)

	profile := &btapb.AppProfile{
		Name: appProfilePath(iac.project, instanceID, profileID),
	}

	if updateAttrs.Description != nil {
		profile.Description = optional.ToString(updateAttrs.Description)
	}

	err := setRoutingPolicy(profile, updateAttrs.RoutingConfig, updateAttrs.RoutingPolicy,
		updateAttrs.ClusterID, updateAttrs.AllowTransactionalWrites, true)
	if err != nil {
		return err
	}

	err = setIsolation(profile, updateAttrs.Isolation)
	if err != nil {
		return err
	}

	patchRequest := &btapb.UpdateAppProfileRequest{
		AppProfile: profile,
		UpdateMask: &field_mask.FieldMask{
			Paths: updateAttrs.GetFieldMaskPath(),
		},
		IgnoreWarnings: updateAttrs.IgnoreWarnings,
	}
	updateRequest, err := iac.iClient.UpdateAppProfile(ctx, patchRequest)
	if err != nil {
		return err
	}

	return longrunning.InternalNewOperation(iac.lroClient, updateRequest).Wait(ctx, nil)

}

// DeleteAppProfile deletes an app profile from an instance.
func (iac *InstanceAdminClient) DeleteAppProfile(ctx context.Context, instanceID, name string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	deleteProfileRequest := &btapb.DeleteAppProfileRequest{
		Name:           "projects/" + iac.project + "/instances/" + instanceID + "/appProfiles/" + name,
		IgnoreWarnings: true,
	}
	_, err := iac.iClient.DeleteAppProfile(ctx, deleteProfileRequest)
	return err

}

// UpdateInstanceResults contains information about the
// changes made after invoking UpdateInstanceAndSyncClusters.
type UpdateInstanceResults struct {
	InstanceUpdated bool
	CreatedClusters []string
	DeletedClusters []string
	UpdatedClusters []string
}

func (r *UpdateInstanceResults) String() string {
	return fmt.Sprintf("Instance updated? %v Clusters added:%v Clusters deleted:%v Clusters updated:%v",
		r.InstanceUpdated, r.CreatedClusters, r.DeletedClusters, r.UpdatedClusters)
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

// UpdateInstanceAndSyncClusters updates an instance and its clusters, and will synchronize the
// clusters in the instance with the provided clusters, creating and deleting them as necessary.
// The provided InstanceWithClustersConfig is used as follows:
//   - InstanceID is required
//   - DisplayName and InstanceType are updated only if they are not empty
//   - ClusterID is required for any provided cluster
//   - Any cluster present in conf.Clusters but not part of the instance will be created using CreateCluster
//     and the given ClusterConfig.
//   - Any cluster missing from conf.Clusters but present in the instance will be removed from the instance
//     using DeleteCluster.
//   - Any cluster in conf.Clusters that also exists in the instance will be
//     updated either to contain the provided number of nodes or to use the
//     provided autoscaling config. If both the number of nodes and autoscaling
//     are configured, autoscaling takes precedence. If the number of nodes is zero
//     and autoscaling is not provided in InstanceWithClustersConfig, the cluster
//     is not updated.
//
// This method may return an error after partially succeeding, for example if the instance is updated
// but a cluster update fails. If an error is returned, InstanceInfo and Clusters may be called to
// determine the current state. The return UpdateInstanceResults will describe the work done by the
// method, whether partial or complete.
func UpdateInstanceAndSyncClusters(ctx context.Context, iac *InstanceAdminClient, conf *InstanceWithClustersConfig) (*UpdateInstanceResults, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)

	// First fetch the existing clusters so we know what to remove, add or update.
	existingClusters, err := iac.Clusters(ctx, conf.InstanceID)
	if err != nil {
		return nil, err
	}

	updatedInstance, err := iac.updateInstance(ctx, conf)
	if err != nil {
		return nil, err
	}

	results := &UpdateInstanceResults{InstanceUpdated: updatedInstance}

	existingClusterNames := make(map[string]bool)
	for _, cluster := range existingClusters {
		existingClusterNames[cluster.Name] = true
	}

	// Synchronize clusters that were passed in with the existing clusters in the instance.
	// First update any cluster we encounter that already exists in the instance.
	// Collect the clusters that we will create and delete so that we can minimize disruption
	// of the instance.
	clustersToCreate := list.New()
	clustersToDelete := list.New()
	for _, cluster := range conf.Clusters {
		_, clusterExists := existingClusterNames[cluster.ClusterID]
		if !clusterExists {
			// The cluster doesn't exist yet, so we must create it.
			clustersToCreate.PushBack(cluster)
			continue
		}
		delete(existingClusterNames, cluster.ClusterID)

		if cluster.NumNodes <= 0 && cluster.AutoscalingConfig == nil {
			// We only synchronize clusters with a valid number of nodes
			// or a valid autoscaling config.
			continue
		}

		// We update the clusters autoscaling config, or its number of serve
		// nodes.
		var updateErr error
		if cluster.AutoscalingConfig != nil {
			updateErr = iac.SetAutoscaling(ctx, conf.InstanceID, cluster.ClusterID,
				*cluster.AutoscalingConfig)
		} else {
			updateErr = iac.UpdateCluster(ctx, conf.InstanceID, cluster.ClusterID,
				cluster.NumNodes)
		}
		if updateErr != nil {
			return results, fmt.Errorf("UpdateCluster %q failed %w; Progress: %v",
				cluster.ClusterID, updateErr, results)
		}
		results.UpdatedClusters = append(results.UpdatedClusters, cluster.ClusterID)
	}

	// Any cluster left in existingClusterNames was NOT in the given config and should be deleted.
	for clusterToDelete := range existingClusterNames {
		clustersToDelete.PushBack(clusterToDelete)
	}

	// Now that we have the clusters that we need to create and delete, we do so keeping the following
	// in mind:
	// - Don't delete the last cluster in the instance, as that will result in an error.
	// - Attempt to offset each deletion with a creation before another deletion, so that instance
	//   capacity is never reduced more than necessary.
	// Note that there is a limit on number of clusters in an instance which we are not aware of here,
	// so delete a cluster before adding one (as long as there are > 1 clusters left) so that we are
	// less likely to exceed the maximum number of clusters.
	numExistingClusters := len(existingClusters)
	nextCreation := clustersToCreate.Front()
	nextDeletion := clustersToDelete.Front()
	for {
		// We are done when both lists are empty.
		if nextCreation == nil && nextDeletion == nil {
			break
		}

		// If there is more than one existing cluster, we always want to delete first if possible.
		// If there are no more creations left, always go ahead with the deletion.
		if (numExistingClusters > 1 && nextDeletion != nil) || nextCreation == nil {
			clusterToDelete := nextDeletion.Value.(string)
			err = iac.DeleteCluster(ctx, conf.InstanceID, clusterToDelete)
			if err != nil {
				return results, fmt.Errorf("DeleteCluster %q failed %w; Progress: %v",
					clusterToDelete, err, results)
			}
			results.DeletedClusters = append(results.DeletedClusters, clusterToDelete)
			numExistingClusters--
			nextDeletion = nextDeletion.Next()
		}

		// Now create a new cluster if required.
		if nextCreation != nil {
			clusterToCreate := nextCreation.Value.(ClusterConfig)
			// Assume the cluster config is well formed and rely on the underlying call to error out.
			// Make sure to set the InstanceID, though, since we know what it must be.
			clusterToCreate.InstanceID = conf.InstanceID
			err = iac.CreateCluster(ctx, &clusterToCreate)
			if err != nil {
				return results, fmt.Errorf("CreateCluster %v failed %w; Progress: %v",
					clusterToCreate, err, results)
			}
			results.CreatedClusters = append(results.CreatedClusters, clusterToCreate.ClusterID)
			numExistingClusters++
			nextCreation = nextCreation.Next()
		}
	}

	return results, nil
}

// RestoreTable creates a table from a backup. The table will be created in the same cluster as the backup.
// To restore a table to a different instance, see RestoreTableFrom.
func (ac *AdminClient) RestoreTable(ctx context.Context, table, cluster, backup string) error {
	return ac.RestoreTableFrom(ctx, ac.instance, table, cluster, backup)
}

// RestoreTableFrom creates a new table in the admin's instance by restoring from the given backup and instance.
// To restore within the same instance, see RestoreTable.
// sourceInstance (ex. "my-instance") and sourceCluster (ex. "my-cluster") are the instance and cluster in which the new table will be restored from.
// tableName (ex. "my-restored-table") will be the name of the newly created table.
// backupName (ex. "my-backup") is the name of the backup to restore.
func (ac *AdminClient) RestoreTableFrom(ctx context.Context, sourceInstance, table, sourceCluster, backup string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	parent := ac.instancePrefix()
	sourceBackupPath := ac.backupPath(sourceCluster, sourceInstance, backup)
	req := &btapb.RestoreTableRequest{
		Parent:  parent,
		TableId: table,
		Source:  &btapb.RestoreTableRequest_Backup{Backup: sourceBackupPath},
	}
	op, err := ac.tClient.RestoreTable(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Table{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

type backupOptions struct {
	backupType        *BackupType
	hotToStandardTime *time.Time
	expireTime        *time.Time
}

// BackupOption can be used to specify parameters for backup operations.
type BackupOption func(*backupOptions)

// WithHotToStandardBackup option can be used to create backup with
// type [BackupTypeHot] and specify time at which the hot backup will be
// converted to a standard backup. Once the 'hotToStandardTime' has passed,
// Cloud Bigtable will convert the hot backup to a standard backup.
// This value must be greater than the backup creation time by at least 24 hours
func WithHotToStandardBackup(hotToStandardTime time.Time) BackupOption {
	return func(bo *backupOptions) {
		btHot := BackupTypeHot
		bo.backupType = &btHot
		bo.hotToStandardTime = &hotToStandardTime
	}
}

// WithExpiry option can be used to create backup
// that expires after time 'expireTime'.
// Once the 'expireTime' has passed, Cloud Bigtable will delete the backup.
func WithExpiry(expireTime time.Time) BackupOption {
	return func(bo *backupOptions) {
		bo.expireTime = &expireTime
	}
}

// WithHotBackup option can be used to create backup
// with type [BackupTypeHot]
func WithHotBackup() BackupOption {
	return func(bo *backupOptions) {
		btHot := BackupTypeHot
		bo.backupType = &btHot
	}
}

// CreateBackup creates a new backup in the specified cluster from the
// specified source table with the user-provided expire time.
func (ac *AdminClient) CreateBackup(ctx context.Context, table, cluster, backup string, expireTime time.Time) error {
	return ac.CreateBackupWithOptions(ctx, table, cluster, backup, WithExpiry(expireTime))
}

// CreateBackupWithOptions is similar to CreateBackup but lets the user specify additional options.
func (ac *AdminClient) CreateBackupWithOptions(ctx context.Context, table, cluster, backup string, opts ...BackupOption) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()

	o := backupOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if o.expireTime == nil {
		return errExpiryMissing
	}
	parsedExpireTime := timestamppb.New(*o.expireTime)

	req := &btapb.CreateBackupRequest{
		Parent:   prefix + "/clusters/" + cluster,
		BackupId: backup,
		Backup: &btapb.Backup{
			ExpireTime:  parsedExpireTime,
			SourceTable: prefix + "/tables/" + table,
		},
	}

	if o.backupType != nil {
		req.Backup.BackupType = btapb.Backup_BackupType(*o.backupType)
	}
	if o.hotToStandardTime != nil {
		req.Backup.HotToStandardTime = timestamppb.New(*o.hotToStandardTime)
	}
	op, err := ac.tClient.CreateBackup(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Backup{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// CopyBackup copies the specified source backup with the user-provided expire time.
func (ac *AdminClient) CopyBackup(ctx context.Context, sourceCluster, sourceBackup,
	destProject, destInstance, destCluster, destBackup string, expireTime time.Time) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	sourceBackupPath := ac.backupPath(sourceCluster, ac.instance, sourceBackup)
	destPrefix := instancePrefix(destProject, destInstance)
	req := &btapb.CopyBackupRequest{
		Parent:       destPrefix + "/clusters/" + destCluster,
		BackupId:     destBackup,
		SourceBackup: sourceBackupPath,
		ExpireTime:   timestamppb.New(expireTime),
	}

	op, err := ac.tClient.CopyBackup(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.Backup{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// Backups returns a BackupIterator for iterating over the backups in a cluster.
// To list backups across all of the clusters in the instance specify "-" as the cluster.
func (ac *AdminClient) Backups(ctx context.Context, cluster string) *BackupIterator {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	prefix := ac.instancePrefix()
	clusterPath := prefix + "/clusters/" + cluster

	it := &BackupIterator{}
	req := &btapb.ListBackupsRequest{
		Parent: clusterPath,
	}

	fetch := func(pageSize int, pageToken string) (string, error) {
		req.PageToken = pageToken
		if pageSize > math.MaxInt32 {
			req.PageSize = math.MaxInt32
		} else {
			req.PageSize = int32(pageSize)
		}

		var resp *btapb.ListBackupsResponse
		err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
			var err error
			resp, err = ac.tClient.ListBackups(ctx, req)
			return err
		}, adminRetryOptions...)
		if err != nil {
			return "", err
		}
		for _, s := range resp.Backups {
			backupInfo, err := newBackupInfo(s)
			if err != nil {
				return "", fmt.Errorf("failed to parse backup proto %w", err)
			}
			it.items = append(it.items, backupInfo)
		}
		return resp.NextPageToken, nil
	}
	bufLen := func() int { return len(it.items) }
	takeBuf := func() interface{} { b := it.items; it.items = nil; return b }

	it.pageInfo, it.nextFunc = iterator.NewPageInfo(fetch, bufLen, takeBuf)

	return it
}

// newBackupInfo creates a BackupInfo struct from a btapb.Backup protocol buffer.
func newBackupInfo(backup *btapb.Backup) (*BackupInfo, error) {
	nameParts := strings.Split(backup.Name, "/")
	name := nameParts[len(nameParts)-1]
	tablePathParts := strings.Split(backup.SourceTable, "/")
	tableID := tablePathParts[len(tablePathParts)-1]

	if err := backup.StartTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid startTime: %v", err)
	}
	startTime := backup.GetStartTime().AsTime()

	if err := backup.EndTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid endTime: %v", err)
	}
	endTime := backup.GetEndTime().AsTime()

	if err := backup.ExpireTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid expireTime: %v", err)
	}
	expireTime := backup.GetExpireTime().AsTime()

	var htsTimePtr *time.Time
	if backup.GetHotToStandardTime() != nil {
		if err := backup.GetHotToStandardTime().CheckValid(); err != nil {
			return nil, fmt.Errorf("invalid HotToStandardTime: %v", err)
		}
		htsTime := backup.GetHotToStandardTime().AsTime()
		htsTimePtr = &htsTime
	}

	encryptionInfo := newEncryptionInfo(backup.EncryptionInfo)
	bi := BackupInfo{
		Name:              name,
		SourceTable:       tableID,
		SourceBackup:      backup.SourceBackup,
		SizeBytes:         backup.SizeBytes,
		StartTime:         startTime,
		EndTime:           endTime,
		ExpireTime:        expireTime,
		State:             backup.State.String(),
		EncryptionInfo:    encryptionInfo,
		BackupType:        BackupType(backup.GetBackupType()),
		HotToStandardTime: htsTimePtr,
	}

	return &bi, nil
}

// BackupIterator is an EntryIterator that iterates over log entries.
type BackupIterator struct {
	items    []*BackupInfo
	pageInfo *iterator.PageInfo
	nextFunc func() error
}

// PageInfo supports pagination. See https://godoc.org/google.golang.org/api/iterator package for details.
func (it *BackupIterator) PageInfo() *iterator.PageInfo {
	return it.pageInfo
}

// Next returns the next result. Its second return value is iterator.Done
// (https://godoc.org/google.golang.org/api/iterator) if there are no more
// results. Once Next returns Done, all subsequent calls will return Done.
func (it *BackupIterator) Next() (*BackupInfo, error) {
	if err := it.nextFunc(); err != nil {
		return nil, err
	}
	item := it.items[0]
	it.items = it.items[1:]
	return item, nil
}

// BackupType denotes the type of the backup.
type BackupType int32

const (
	// BackupTypeUnspecified denotes that backup type has not been specified.
	BackupTypeUnspecified BackupType = 0

	// BackupTypeStandard is the default type for Cloud Bigtable managed backups. Supported for
	// backups created in both HDD and SSD instances. Requires optimization when
	// restored to a table in an SSD instance.
	BackupTypeStandard BackupType = 1

	// BackupTypeHot is a backup type with faster restore to SSD performance. Only supported for
	// backups created in SSD instances. A new SSD table restored from a hot
	// backup reaches production performance more quickly than a standard
	// backup.
	BackupTypeHot BackupType = 2
)

// BackupInfo contains backup metadata. This struct is read-only.
type BackupInfo struct {
	Name           string
	SourceTable    string
	SourceBackup   string
	SizeBytes      int64
	StartTime      time.Time
	EndTime        time.Time
	ExpireTime     time.Time
	State          string
	EncryptionInfo *EncryptionInfo
	BackupType     BackupType

	// The time at which the hot backup will be converted to a standard backup.
	// Once the `hot_to_standard_time` has passed, Cloud Bigtable will convert the
	// hot backup to a standard backup. This value must be greater than the backup
	// creation time by at least 24 hours
	//
	// This field only applies for hot backups.
	HotToStandardTime *time.Time
}

// BackupInfo gets backup metadata.
func (ac *AdminClient) BackupInfo(ctx context.Context, cluster, backup string) (*BackupInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	backupPath := ac.backupPath(cluster, ac.instance, backup)

	req := &btapb.GetBackupRequest{
		Name: backupPath,
	}

	var resp *btapb.Backup
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		resp, err = ac.tClient.GetBackup(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	return newBackupInfo(resp)
}

// DeleteBackup deletes a backup in a cluster.
func (ac *AdminClient) DeleteBackup(ctx context.Context, cluster, backup string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	backupPath := ac.backupPath(cluster, ac.instance, backup)

	req := &btapb.DeleteBackupRequest{
		Name: backupPath,
	}
	_, err := ac.tClient.DeleteBackup(ctx, req)
	return err
}

// UpdateBackup updates the backup metadata in a cluster. The API only supports updating expire time.
func (ac *AdminClient) UpdateBackup(ctx context.Context, cluster, backup string, expireTime time.Time) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	backupPath := ac.backupPath(cluster, ac.instance, backup)

	expireTimestamp := timestamppb.New(expireTime)

	updateMask := &field_mask.FieldMask{}
	updateMask.Paths = append(updateMask.Paths, "expire_time")

	req := &btapb.UpdateBackupRequest{
		Backup: &btapb.Backup{
			Name:       backupPath,
			ExpireTime: expireTimestamp,
		},
		UpdateMask: updateMask,
	}
	_, err := ac.tClient.UpdateBackup(ctx, req)
	return err
}

// UpdateBackupHotToStandardTime updates the HotToStandardTime of a hot backup.
func (ac *AdminClient) UpdateBackupHotToStandardTime(ctx context.Context, cluster, backup string, hotToStandardTime time.Time) error {
	return ac.updateBackupHotToStandardTime(ctx, cluster, backup, &hotToStandardTime)
}

// UpdateBackupRemoveHotToStandardTime removes the HotToStandardTime of a hot backup.
func (ac *AdminClient) UpdateBackupRemoveHotToStandardTime(ctx context.Context, cluster, backup string) error {
	return ac.updateBackupHotToStandardTime(ctx, cluster, backup, nil)
}

func (ac *AdminClient) updateBackupHotToStandardTime(ctx context.Context, cluster, backup string, hotToStandardTime *time.Time) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	backupPath := ac.backupPath(cluster, ac.instance, backup)

	updateMask := &field_mask.FieldMask{}
	updateMask.Paths = append(updateMask.Paths, "hot_to_standard_time")

	req := &btapb.UpdateBackupRequest{
		Backup: &btapb.Backup{
			Name: backupPath,
		},
		UpdateMask: updateMask,
	}

	if hotToStandardTime != nil {
		req.Backup.HotToStandardTime = timestamppb.New(*hotToStandardTime)
	}

	_, err := ac.tClient.UpdateBackup(ctx, req)
	return err
}

// AuthorizedViewConf contains information about an authorized view.
type AuthorizedViewConf struct {
	TableID          string
	AuthorizedViewID string

	// Types that are valid to be assigned to AuthorizedView:
	//  *SubsetViewConf
	AuthorizedView     isAuthorizedView
	DeletionProtection DeletionProtection
}

// A private interface that currently only implemented by SubsetViewConf, ensuring that only SubsetViewConf instances are accepted as an AuthorizedView.
// In the future if a new type of AuthorizedView is introduced, it should also implements this interface.
type isAuthorizedView interface {
	isAuthorizedView()
}

func (av AuthorizedViewConf) proto() *btapb.AuthorizedView {
	var avp btapb.AuthorizedView

	switch dp := av.DeletionProtection; dp {
	case Protected:
		avp.DeletionProtection = true
	case Unprotected:
		avp.DeletionProtection = false
	default:
		break
	}

	switch avt := av.AuthorizedView.(type) {
	case *SubsetViewConf:
		avp.AuthorizedView = &btapb.AuthorizedView_SubsetView_{
			SubsetView: avt.proto(),
		}
	default:
		break
	}
	return &avp
}

// FamilySubset represents a subset of a column family.
type FamilySubset struct {
	Qualifiers        [][]byte
	QualifierPrefixes [][]byte
}

// SubsetViewConf contains configuration specific to an authorized view of subset view type.
type SubsetViewConf struct {
	RowPrefixes   [][]byte
	FamilySubsets map[string]FamilySubset
}

func (*SubsetViewConf) isAuthorizedView() {}

// AddRowPrefix adds a new row prefix to the subset view.
func (s *SubsetViewConf) AddRowPrefix(prefix []byte) {
	s.RowPrefixes = append(s.RowPrefixes, prefix)
}

func (s *SubsetViewConf) getOrCreateFamilySubset(familyName string) FamilySubset {
	if s.FamilySubsets == nil {
		s.FamilySubsets = make(map[string]FamilySubset)
	}
	if _, ok := s.FamilySubsets[familyName]; !ok {
		s.FamilySubsets[familyName] = FamilySubset{}
	}
	return s.FamilySubsets[familyName]
}

func (s SubsetViewConf) proto() *btapb.AuthorizedView_SubsetView {
	var p btapb.AuthorizedView_SubsetView
	p.RowPrefixes = append(p.RowPrefixes, s.RowPrefixes...)
	if p.FamilySubsets == nil {
		p.FamilySubsets = make(map[string]*btapb.AuthorizedView_FamilySubsets)
	}
	for familyName, subset := range s.FamilySubsets {
		p.FamilySubsets[familyName] = &btapb.AuthorizedView_FamilySubsets{
			Qualifiers:        subset.Qualifiers,
			QualifierPrefixes: subset.QualifierPrefixes,
		}
	}
	return &p
}

// AddFamilySubsetQualifier adds an individual column qualifier to be included in a subset view.
func (s *SubsetViewConf) AddFamilySubsetQualifier(familyName string, qualifier []byte) {
	fs := s.getOrCreateFamilySubset(familyName)
	fs.Qualifiers = append(fs.Qualifiers, qualifier)
	s.FamilySubsets[familyName] = fs
}

// AddFamilySubsetQualifierPrefix adds a prefix for column qualifiers to be included in a subset view.
func (s *SubsetViewConf) AddFamilySubsetQualifierPrefix(familyName string, qualifierPrefix []byte) {
	fs := s.getOrCreateFamilySubset(familyName)
	fs.QualifierPrefixes = append(fs.QualifierPrefixes, qualifierPrefix)
	s.FamilySubsets[familyName] = fs
}

// Authorized Views

// CreateAuthorizedView creates a new authorized view in a table.
func (ac *AdminClient) CreateAuthorizedView(ctx context.Context, conf *AuthorizedViewConf) error {
	if conf.TableID == "" || conf.AuthorizedViewID == "" {
		return errors.New("both AuthorizedViewID and TableID are required")
	}
	if _, ok := conf.AuthorizedView.(*SubsetViewConf); !ok {
		return errors.New("SubsetView must be specified in AuthorizedViewConf")
	}

	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.CreateAuthorizedViewRequest{
		Parent:           fmt.Sprintf("%s/tables/%s", ac.instancePrefix(), conf.TableID),
		AuthorizedViewId: conf.AuthorizedViewID,
		AuthorizedView:   conf.proto(),
	}
	_, err := ac.tClient.CreateAuthorizedView(ctx, req)
	return err
}

// AuthorizedViewInfo contains authorized view metadata. This struct is read-only.
type AuthorizedViewInfo struct {
	TableID          string
	AuthorizedViewID string

	AuthorizedView     isAuthorizedViewInfo
	DeletionProtection DeletionProtection
}

type isAuthorizedViewInfo interface {
	isAuthorizedViewInfo()
}

// SubsetViewInfo contains read-only SubsetView metadata.
type SubsetViewInfo struct {
	RowPrefixes   [][]byte
	FamilySubsets map[string]FamilySubset
}

func (*SubsetViewInfo) isAuthorizedViewInfo() {}

func (s *SubsetViewInfo) fillInfo(internal *btapb.AuthorizedView_SubsetView) {
	s.RowPrefixes = [][]byte{}
	s.RowPrefixes = append(s.RowPrefixes, internal.RowPrefixes...)
	if s.FamilySubsets == nil {
		s.FamilySubsets = make(map[string]FamilySubset)
	}
	for k, v := range internal.FamilySubsets {
		s.FamilySubsets[k] = FamilySubset{
			Qualifiers:        v.Qualifiers,
			QualifierPrefixes: v.QualifierPrefixes,
		}
	}
}

// AuthorizedViewInfo retrieves information about an authorized view.
func (ac *AdminClient) AuthorizedViewInfo(ctx context.Context, tableID, authorizedViewID string) (*AuthorizedViewInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.GetAuthorizedViewRequest{
		Name: fmt.Sprintf("%s/tables/%s/authorizedViews/%s", ac.instancePrefix(), tableID, authorizedViewID),
	}
	var res *btapb.AuthorizedView

	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.GetAuthorizedView(ctx, req)
		return err
	}, adminRetryOptions...)

	if err != nil {
		return nil, err
	}

	av := &AuthorizedViewInfo{TableID: tableID, AuthorizedViewID: authorizedViewID}
	if res.DeletionProtection {
		av.DeletionProtection = Protected
	} else {
		av.DeletionProtection = Unprotected
	}
	if res.GetSubsetView() != nil {
		s := SubsetViewInfo{}
		s.fillInfo(res.GetSubsetView())
		av.AuthorizedView = &s
	}
	return av, nil
}

// AuthorizedViews returns a list of the authorized views in the table.
func (ac *AdminClient) AuthorizedViews(ctx context.Context, tableID string) ([]string, error) {
	names := []string{}
	prefix := fmt.Sprintf("%s/tables/%s", ac.instancePrefix(), tableID)

	req := &btapb.ListAuthorizedViewsRequest{
		Parent: prefix,
		View:   btapb.AuthorizedView_NAME_ONLY,
	}
	var res *btapb.ListAuthorizedViewsResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.ListAuthorizedViews(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	for _, av := range res.AuthorizedViews {
		names = append(names, strings.TrimPrefix(av.Name, prefix+"/authorizedViews/"))
	}
	return names, nil
}

// UpdateAuthorizedViewConf contains all the information necessary to update or partial update an authorized view.
type UpdateAuthorizedViewConf struct {
	AuthorizedViewConf AuthorizedViewConf
	IgnoreWarnings     bool
}

// UpdateAuthorizedView updates an authorized view in a table according to the given configuration.
func (ac *AdminClient) UpdateAuthorizedView(ctx context.Context, conf UpdateAuthorizedViewConf) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	if conf.AuthorizedViewConf.TableID == "" || conf.AuthorizedViewConf.AuthorizedViewID == "" {
		return errors.New("both AuthorizedViewID and TableID is required")
	}
	av := conf.AuthorizedViewConf.proto()
	av.Name = ac.authorizedViewPath(conf.AuthorizedViewConf.TableID, conf.AuthorizedViewConf.AuthorizedViewID)

	updateMask := &field_mask.FieldMask{
		Paths: []string{},
	}
	if conf.AuthorizedViewConf.DeletionProtection != None {
		updateMask.Paths = append(updateMask.Paths, "deletion_protection")
	}
	if _, ok := conf.AuthorizedViewConf.AuthorizedView.(*SubsetViewConf); ok {
		updateMask.Paths = append(updateMask.Paths, "subset_view")
	}
	req := &btapb.UpdateAuthorizedViewRequest{
		AuthorizedView: av,
		UpdateMask:     updateMask,
		IgnoreWarnings: conf.IgnoreWarnings,
	}
	lro, err := ac.tClient.UpdateAuthorizedView(ctx, req)
	if err != nil {
		return fmt.Errorf("error from update authorized view: %w", err)
	}
	var res btapb.AuthorizedView
	op := longrunning.InternalNewOperation(ac.lroClient, lro)
	if err = op.Wait(ctx, &res); err != nil {
		return fmt.Errorf("error from operation: %v", err)
	}
	return nil
}

// DeleteAuthorizedView deletes an authorized view in a table.
func (ac *AdminClient) DeleteAuthorizedView(ctx context.Context, tableID, authorizedViewID string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.DeleteAuthorizedViewRequest{
		Name: ac.authorizedViewPath(tableID, authorizedViewID),
	}
	_, err := ac.tClient.DeleteAuthorizedView(ctx, req)
	return err
}

// Logical Views

// CreateLogicalView creates a new logical view in an instance.
func (iac *InstanceAdminClient) CreateLogicalView(ctx context.Context, instanceID string, conf *LogicalViewInfo) error {
	if conf.LogicalViewID == "" {
		return errors.New("LogicalViewID is required")
	}

	lv := &btapb.LogicalView{
		Query: conf.Query,
	}
	if conf.DeletionProtection != None {
		switch dp := conf.DeletionProtection; dp {
		case Protected:
			lv.DeletionProtection = true
		case Unprotected:
			lv.DeletionProtection = false
		default:
			break
		}
	}

	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.CreateLogicalViewRequest{
		Parent:        instancePrefix(iac.project, instanceID),
		LogicalViewId: conf.LogicalViewID,
		LogicalView:   lv,
	}

	op, err := iac.iClient.CreateLogicalView(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.LogicalView{}
	return longrunning.InternalNewOperation(iac.lroClient, op).Wait(ctx, &resp)
}

// LogicalViewInfo contains logical view metadata. This struct is read-only.
type LogicalViewInfo struct {
	LogicalViewID string

	Query              string
	DeletionProtection DeletionProtection
}

// LogicalViewInfo retrieves information about a logical view.
func (iac *InstanceAdminClient) LogicalViewInfo(ctx context.Context, instanceID, logicalViewID string) (*LogicalViewInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	prefix := instancePrefix(iac.project, instanceID)
	req := &btapb.GetLogicalViewRequest{
		Name: logicalViewPath(iac.project, instanceID, logicalViewID),
	}
	var res *btapb.LogicalView

	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.GetLogicalView(ctx, req)
		return err
	}, adminRetryOptions...)

	if err != nil {
		return nil, err
	}
	lv := &LogicalViewInfo{LogicalViewID: strings.TrimPrefix(res.Name, prefix+"/logicalViews/"), Query: res.Query}
	if res.DeletionProtection {
		lv.DeletionProtection = Protected
	} else {
		lv.DeletionProtection = Unprotected
	}
	return lv, nil
}

// LogicalViews returns a list of the logical views in the instance.
func (iac *InstanceAdminClient) LogicalViews(ctx context.Context, instanceID string) ([]LogicalViewInfo, error) {
	views := []LogicalViewInfo{}
	prefix := instancePrefix(iac.project, instanceID)
	req := &btapb.ListLogicalViewsRequest{
		Parent: prefix,
	}
	var res *btapb.ListLogicalViewsResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.ListLogicalViews(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	for _, lView := range res.LogicalViews {
		lv := LogicalViewInfo{LogicalViewID: strings.TrimPrefix(lView.Name, prefix+"/logicalViews/"), Query: lView.Query}
		if lView.DeletionProtection {
			lv.DeletionProtection = Protected
		} else {
			lv.DeletionProtection = Unprotected
		}
		views = append(views, lv)
	}
	return views, nil
}

// UpdateLogicalView updates a logical view in an instance according to the given configuration.
func (iac *InstanceAdminClient) UpdateLogicalView(ctx context.Context, instanceID string, conf LogicalViewInfo) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	if conf.LogicalViewID == "" {
		return errors.New("LogicalViewID is required")
	}
	lv := &btapb.LogicalView{}
	lv.Name = logicalViewPath(iac.project, instanceID, conf.LogicalViewID)

	updateMask := &field_mask.FieldMask{
		Paths: []string{},
	}
	if conf.Query != "" {
		updateMask.Paths = append(updateMask.Paths, "query")
		lv.Query = conf.Query
	}
	if conf.DeletionProtection != None {
		updateMask.Paths = append(updateMask.Paths, "deletion_protection")
		switch dp := conf.DeletionProtection; dp {
		case Protected:
			lv.DeletionProtection = true
		case Unprotected:
			lv.DeletionProtection = false
		default:
			break
		}
	}
	req := &btapb.UpdateLogicalViewRequest{
		LogicalView: lv,
		UpdateMask:  updateMask,
	}
	lro, err := iac.iClient.UpdateLogicalView(ctx, req)
	if err != nil {
		return fmt.Errorf("error from update logical view: %w", err)
	}
	var res btapb.LogicalView
	op := longrunning.InternalNewOperation(iac.lroClient, lro)
	if err = op.Wait(ctx, &res); err != nil {
		return fmt.Errorf("error from operation: %v", err)
	}
	return nil
}

// DeleteLogicalView deletes a logical view in an instance.
func (iac *InstanceAdminClient) DeleteLogicalView(ctx context.Context, instanceID, logicalViewID string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteLogicalViewRequest{
		Name: logicalViewPath(iac.project, instanceID, logicalViewID),
	}
	_, err := iac.iClient.DeleteLogicalView(ctx, req)
	return err
}

// Materialized Views

// CreateMaterializedView creates a new materialized view in an instance.
func (iac *InstanceAdminClient) CreateMaterializedView(ctx context.Context, instanceID string, conf *MaterializedViewInfo) error {
	if conf.MaterializedViewID == "" {
		return errors.New("MaterializedViewID is required")
	}

	ctx = mergeOutgoingMetadata(ctx, iac.md)
	mv := &btapb.MaterializedView{
		Query: conf.Query,
	}
	if conf.DeletionProtection != None {
		switch dp := conf.DeletionProtection; dp {
		case Protected:
			mv.DeletionProtection = true
		case Unprotected:
			mv.DeletionProtection = false
		default:
			break
		}
	}
	req := &btapb.CreateMaterializedViewRequest{
		Parent:             instancePrefix(iac.project, instanceID),
		MaterializedViewId: conf.MaterializedViewID,
		MaterializedView:   mv,
	}
	op, err := iac.iClient.CreateMaterializedView(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.MaterializedView{}
	return longrunning.InternalNewOperation(iac.lroClient, op).Wait(ctx, &resp)
}

// MaterializedViewInfo contains materialized view metadata. This struct is read-only.
type MaterializedViewInfo struct {
	MaterializedViewID string

	Query              string
	DeletionProtection DeletionProtection
}

// MaterializedViewInfo retrieves information about a materialized view.
func (iac *InstanceAdminClient) MaterializedViewInfo(ctx context.Context, instanceID, materializedViewID string) (*MaterializedViewInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	prefix := instancePrefix(iac.project, instanceID)
	req := &btapb.GetMaterializedViewRequest{
		Name: materializedlViewPath(iac.project, instanceID, materializedViewID),
	}
	var res *btapb.MaterializedView

	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.GetMaterializedView(ctx, req)
		return err
	}, adminRetryOptions...)

	if err != nil {
		return nil, err
	}
	mv := &MaterializedViewInfo{MaterializedViewID: strings.TrimPrefix(res.Name, prefix+"/materializedViews/"), Query: res.Query}
	if res.DeletionProtection {
		mv.DeletionProtection = Protected
	} else {
		mv.DeletionProtection = Unprotected
	}
	return mv, nil
}

// MaterializedViews returns a list of the materialized views in the instance.
func (iac *InstanceAdminClient) MaterializedViews(ctx context.Context, instanceID string) ([]MaterializedViewInfo, error) {
	views := []MaterializedViewInfo{}
	prefix := instancePrefix(iac.project, instanceID)
	req := &btapb.ListMaterializedViewsRequest{
		Parent: prefix,
	}
	var res *btapb.ListMaterializedViewsResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = iac.iClient.ListMaterializedViews(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	for _, mView := range res.MaterializedViews {
		mv := MaterializedViewInfo{MaterializedViewID: strings.TrimPrefix(mView.Name, prefix+"/materializedViews/"), Query: mView.Query}
		if mView.DeletionProtection {
			mv.DeletionProtection = Protected
		} else {
			mv.DeletionProtection = Unprotected
		}
		views = append(views, mv)
	}
	return views, nil
}

// UpdateMaterializedView updates a materialized view in an instance according to the given configuration.
func (iac *InstanceAdminClient) UpdateMaterializedView(ctx context.Context, instanceID string, conf MaterializedViewInfo) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	if conf.MaterializedViewID == "" {
		return errors.New("MaterializedViewID is required")
	}
	mv := &btapb.MaterializedView{}
	mv.Name = materializedlViewPath(iac.project, instanceID, conf.MaterializedViewID)

	updateMask := &field_mask.FieldMask{
		Paths: []string{},
	}
	if conf.Query != "" {
		updateMask.Paths = append(updateMask.Paths, "query")
		mv.Query = conf.Query
	}
	if conf.DeletionProtection != None {
		updateMask.Paths = append(updateMask.Paths, "deletion_protection")
		switch dp := conf.DeletionProtection; dp {
		case Protected:
			mv.DeletionProtection = true
		case Unprotected:
			mv.DeletionProtection = false
		default:
			break
		}
	}
	req := &btapb.UpdateMaterializedViewRequest{
		MaterializedView: mv,
		UpdateMask:       updateMask,
	}
	lro, err := iac.iClient.UpdateMaterializedView(ctx, req)
	if err != nil {
		return fmt.Errorf("error from update materialized view: %w", err)
	}
	var res btapb.MaterializedView
	op := longrunning.InternalNewOperation(iac.lroClient, lro)
	if err = op.Wait(ctx, &res); err != nil {
		return fmt.Errorf("error from operation: %v", err)
	}
	return nil
}

// DeleteMaterializedView deletes a materialized view in an instance.
func (iac *InstanceAdminClient) DeleteMaterializedView(ctx context.Context, instanceID, materializedViewID string) error {
	ctx = mergeOutgoingMetadata(ctx, iac.md)
	req := &btapb.DeleteMaterializedViewRequest{
		Name: materializedlViewPath(iac.project, instanceID, materializedViewID),
	}
	_, err := iac.iClient.DeleteMaterializedView(ctx, req)
	return err
}

// SchemaBundles

// SchemaBundleConf contains the information necessary to create or update a schema bundle.
type SchemaBundleConf struct {
	TableID        string
	SchemaBundleID string
	ProtoSchema    *ProtoSchemaInfo

	// Etag is used for optimistic concurrency control during updates.
	// Ignored during creation.
	Etag string
}

// ProtoSchemaInfo represents a protobuf schema.
type ProtoSchemaInfo struct {
	// Contains a protobuf-serialized
	// [google.protobuf.FileDescriptorSet](https://github.com/protocolbuffers/protobuf/blob/main/src/google/protobuf/descriptor.proto),
	// which could include multiple proto files.
	ProtoDescriptors []byte
}

// CreateSchemaBundle creates a new schema bundle in a table.
func (ac *AdminClient) CreateSchemaBundle(ctx context.Context, conf *SchemaBundleConf) error {
	if conf.TableID == "" || conf.SchemaBundleID == "" {
		return errors.New("both SchemaBundleID and TableID are required in SchemaBundleConf")
	}
	schemaBundle := &btapb.SchemaBundle{}
	if len(conf.ProtoSchema.ProtoDescriptors) > 0 {
		schemaBundle.Type = &btapb.SchemaBundle_ProtoSchema{
			ProtoSchema: &btapb.ProtoSchema{
				ProtoDescriptors: conf.ProtoSchema.ProtoDescriptors,
			},
		}
	}

	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.CreateSchemaBundleRequest{
		Parent:         fmt.Sprintf("%s/tables/%s", ac.instancePrefix(), conf.TableID),
		SchemaBundleId: conf.SchemaBundleID,
		SchemaBundle:   schemaBundle,
	}
	op, err := ac.tClient.CreateSchemaBundle(ctx, req)
	if err != nil {
		return err
	}
	resp := btapb.SchemaBundle{}
	return longrunning.InternalNewOperation(ac.lroClient, op).Wait(ctx, &resp)
}

// SchemaBundleInfo represents information about a schema bundle. Schema bundle is a named collection of related schemas.
// This struct is read-only.
type SchemaBundleInfo struct {
	TableID        string
	SchemaBundleID string

	Etag         string
	SchemaBundle []byte
}

// GetSchemaBundle retrieves information about a schema bundle.
func (ac *AdminClient) GetSchemaBundle(ctx context.Context, tableID, schemaBundleID string) (*SchemaBundleInfo, error) {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.GetSchemaBundleRequest{
		Name: ac.schemaBundlePath(tableID, schemaBundleID),
	}
	var res *btapb.SchemaBundle

	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.GetSchemaBundle(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	sb := &SchemaBundleInfo{
		TableID:        tableID,
		SchemaBundleID: schemaBundleID,
		Etag:           res.Etag,
	}
	if len(res.GetProtoSchema().GetProtoDescriptors()) > 0 {
		sb.SchemaBundle = res.GetProtoSchema().GetProtoDescriptors()
	}

	return sb, nil
}

// SchemaBundles returns a list of the schema bundles in the table.
func (ac *AdminClient) SchemaBundles(ctx context.Context, tableID string) ([]string, error) {
	names := []string{}
	prefix := fmt.Sprintf("%s/tables/%s", ac.instancePrefix(), tableID)

	req := &btapb.ListSchemaBundlesRequest{
		Parent: prefix,
	}
	var res *btapb.ListSchemaBundlesResponse
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		res, err = ac.tClient.ListSchemaBundles(ctx, req)
		return err
	}, adminRetryOptions...)
	if err != nil {
		return nil, err
	}

	for _, res := range res.SchemaBundles {
		names = append(names, strings.TrimPrefix(res.Name, prefix+"/schemaBundles/"))
	}
	return names, nil
}

// UpdateSchemaBundleConf contains all the information necessary to update or partial update a schema bundle.
type UpdateSchemaBundleConf struct {
	SchemaBundleConf SchemaBundleConf
	IgnoreWarnings   bool
}

// UpdateSchemaBundle updates a schema bundle in a table according to the given configuration.
func (ac *AdminClient) UpdateSchemaBundle(ctx context.Context, conf UpdateSchemaBundleConf) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	if conf.SchemaBundleConf.TableID == "" || conf.SchemaBundleConf.SchemaBundleID == "" {
		return errors.New("both SchemaBundleID and TableID is required")
	}
	sb := &btapb.SchemaBundle{
		Name: ac.schemaBundlePath(conf.SchemaBundleConf.TableID, conf.SchemaBundleConf.SchemaBundleID),
		Etag: conf.SchemaBundleConf.Etag,
	}

	updateMask := &field_mask.FieldMask{
		Paths: []string{},
	}
	if len(conf.SchemaBundleConf.ProtoSchema.ProtoDescriptors) > 0 {
		sb.Type = &btapb.SchemaBundle_ProtoSchema{
			ProtoSchema: &btapb.ProtoSchema{
				ProtoDescriptors: conf.SchemaBundleConf.ProtoSchema.ProtoDescriptors,
			},
		}
		updateMask.Paths = append(updateMask.Paths, "proto_schema")
	}
	req := &btapb.UpdateSchemaBundleRequest{
		SchemaBundle:   sb,
		UpdateMask:     updateMask,
		IgnoreWarnings: conf.IgnoreWarnings,
	}
	lro, err := ac.tClient.UpdateSchemaBundle(ctx, req)
	if err != nil {
		return fmt.Errorf("error from update schema bundle: %w", err)
	}
	var res btapb.SchemaBundle
	op := longrunning.InternalNewOperation(ac.lroClient, lro)
	if err = op.Wait(ctx, &res); err != nil {
		return fmt.Errorf("error from operation: %v", err)
	}
	return nil
}

// DeleteSchemaBundle deletes a schema bundle in a table.
func (ac *AdminClient) DeleteSchemaBundle(ctx context.Context, tableID, schemaBundleID string) error {
	ctx = mergeOutgoingMetadata(ctx, ac.md)
	req := &btapb.DeleteSchemaBundleRequest{
		Name: ac.schemaBundlePath(tableID, schemaBundleID),
	}
	_, err := ac.tClient.DeleteSchemaBundle(ctx, req)
	return err
}
