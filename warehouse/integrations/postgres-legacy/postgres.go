package postgreslegacy

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/logfield"

	"golang.org/x/exp/slices"

	"github.com/lib/pq"
	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-server/services/filemanager"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/warehouse/client"
	"github.com/rudderlabs/rudder-server/warehouse/tunnelling"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

const (
	host     = "host"
	dbName   = "database"
	user     = "user"
	password = "password"
	port     = "port"
	sslMode  = "sslMode"
	verifyCA = "verify-ca"
)

const (
	provider       = warehouseutils.POSTGRES
	tableNameLimit = 127
)

// load table transaction stages
const (
	createStagingTable       = "staging_table_creation"
	copyInSchemaStagingTable = "staging_table_copy_in_schema"
	openLoadFiles            = "load_files_opening"
	readGzipLoadFiles        = "load_files_gzip_reading"
	readCsvLoadFiles         = "load_files_csv_reading"
	csvColumnCountMismatch   = "csv_column_count_mismatch"
	loadStagingTable         = "staging_table_loading"
	stagingTableloadStage    = "staging_table_load_stage"
	deleteDedup              = "dedup_deletion"
	insertDedup              = "dedup_insertion"
	dedupStage               = "dedup_stage"
)

var errorsMappings = []model.JobError{
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`dial tcp: lookup .*: no such host`),
	},
	{
		Type:   model.PermissionError,
		Format: regexp.MustCompile(`dial tcp .* connect: connection refused`),
	},
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`pq: database .* does not exist`),
	},
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`pq: the database system is starting up`),
	},
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`pq: the database system is shutting down`),
	},
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`pq: relation .* does not exist`),
	},
	{
		Type:   model.ResourceNotFoundError,
		Format: regexp.MustCompile(`pq: cannot set transaction read-write mode during recovery`),
	},
	{
		Type:   model.ColumnCountError,
		Format: regexp.MustCompile(`pq: tables can have at most 1600 columns`),
	},
	{
		Type:   model.PermissionError,
		Format: regexp.MustCompile(`pq: password authentication failed for user`),
	},
	{
		Type:   model.PermissionError,
		Format: regexp.MustCompile(`pq: permission denied`),
	},
}

var rudderDataTypesMapToPostgres = map[string]string{
	"int":      "bigint",
	"float":    "numeric",
	"string":   "text",
	"datetime": "timestamptz",
	"boolean":  "boolean",
	"json":     "jsonb",
}

var postgresDataTypesMapToRudder = map[string]string{
	"integer":                  "int",
	"smallint":                 "int",
	"bigint":                   "int",
	"double precision":         "float",
	"numeric":                  "float",
	"real":                     "float",
	"text":                     "string",
	"varchar":                  "string",
	"char":                     "string",
	"timestamptz":              "datetime",
	"timestamp with time zone": "datetime",
	"timestamp":                "datetime",
	"boolean":                  "boolean",
	"jsonb":                    "json",
}

type Postgres struct {
	DB                                          *sqlmiddleware.DB
	Namespace                                   string
	ObjectStorage                               string
	Warehouse                                   model.Warehouse
	Uploader                                    warehouseutils.Uploader
	ConnectTimeout                              time.Duration
	logger                                      logger.Logger
	SkipComputingUserLatestTraits               bool
	EnableSQLStatementExecutionPlan             bool
	TxnRollbackTimeout                          time.Duration
	EnableDeleteByJobs                          bool
	SkipComputingUserLatestTraitsWorkspaceIDs   []string
	EnableSQLStatementExecutionPlanWorkspaceIDs []string
	SlowQueryThreshold                          time.Duration
}

func (pg *Postgres) getNewMiddleWare(db *sql.DB) *sqlmiddleware.DB {
	middleware := sqlmiddleware.New(
		db,
		sqlmiddleware.WithLogger(pg.logger),
		sqlmiddleware.WithKeyAndValues(
			logfield.SourceID, pg.Warehouse.Source.ID,
			logfield.SourceType, pg.Warehouse.Source.SourceDefinition.Name,
			logfield.DestinationID, pg.Warehouse.Destination.ID,
			logfield.DestinationType, pg.Warehouse.Destination.DestinationDefinition.Name,
			logfield.WorkspaceID, pg.Warehouse.WorkspaceID,
			logfield.Schema, pg.Namespace,
		),
		sqlmiddleware.WithSlowQueryThreshold(pg.SlowQueryThreshold),
	)
	return middleware
}

type Credentials struct {
	Host       string
	DBName     string
	User       string
	Password   string
	Port       string
	SSLMode    string
	SSLDir     string
	TunnelInfo *tunnelling.TunnelInfo
	timeout    time.Duration
}

var primaryKeyMap = map[string]string{
	warehouseutils.UsersTable:      "id",
	warehouseutils.IdentifiesTable: "id",
	warehouseutils.DiscardsTable:   "row_id",
}

var partitionKeyMap = map[string]string{
	warehouseutils.UsersTable:      "id",
	warehouseutils.IdentifiesTable: "id",
	warehouseutils.DiscardsTable:   "row_id, column_name, table_name",
}

func New() *Postgres {
	return &Postgres{
		logger: logger.NewLogger().Child("warehouse").Child("integrations").Child("postgres"),
	}
}

func WithConfig(h *Postgres, config *config.Config) {
	h.SkipComputingUserLatestTraits = config.GetBool("Warehouse.postgres.skipComputingUserLatestTraits", false)
	h.TxnRollbackTimeout = config.GetDuration("Warehouse.postgres.txnRollbackTimeout", 30, time.Second)
	h.EnableSQLStatementExecutionPlan = config.GetBool("Warehouse.postgres.enableSQLStatementExecutionPlan", false)
	h.EnableDeleteByJobs = config.GetBool("Warehouse.postgres.enableDeleteByJobs", false)
	h.SkipComputingUserLatestTraitsWorkspaceIDs = config.GetStringSlice("Warehouse.postgres.SkipComputingUserLatestTraitsWorkspaceIDs", nil)
	h.EnableSQLStatementExecutionPlanWorkspaceIDs = config.GetStringSlice("Warehouse.postgres.EnableSQLStatementExecutionPlanWorkspaceIDs", nil)
	h.SlowQueryThreshold = config.GetDuration("Warehouse.postgres.slowQueryThreshold", 5, time.Minute)
}

func (pg *Postgres) connect() (*sqlmiddleware.DB, error) {
	cred := pg.getConnectionCredentials()
	dsn := url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%s", cred.Host, cred.Port),
		User:   url.UserPassword(cred.User, cred.Password),
		Path:   cred.DBName,
	}

	values := url.Values{}
	values.Add("sslmode", cred.SSLMode)

	if cred.timeout > 0 {
		values.Add("connect_timeout", fmt.Sprintf("%d", cred.timeout/time.Second))
	}

	if cred.SSLMode == verifyCA {
		values.Add("sslrootcert", fmt.Sprintf("%s/server-ca.pem", cred.SSLDir))
		values.Add("sslcert", fmt.Sprintf("%s/client-cert.pem", cred.SSLDir))
		values.Add("sslkey", fmt.Sprintf("%s/client-key.pem", cred.SSLDir))
	}

	dsn.RawQuery = values.Encode()

	var (
		err error
		db  *sql.DB
	)

	if cred.TunnelInfo != nil {

		db, err = tunnelling.SQLConnectThroughTunnel(dsn.String(), cred.TunnelInfo.Config)
		if err != nil {
			return nil, fmt.Errorf("opening connection to postgres through tunnelling: %w", err)
		}
		return pg.getNewMiddleWare(db), nil
	}

	if db, err = sql.Open("postgres", dsn.String()); err != nil {
		return nil, fmt.Errorf("opening connection to postgres: %w", err)
	}

	return pg.getNewMiddleWare(db), nil
}

func (pg *Postgres) getConnectionCredentials() Credentials {
	sslMode := warehouseutils.GetConfigValue(sslMode, pg.Warehouse)
	creds := Credentials{
		Host:     warehouseutils.GetConfigValue(host, pg.Warehouse),
		DBName:   warehouseutils.GetConfigValue(dbName, pg.Warehouse),
		User:     warehouseutils.GetConfigValue(user, pg.Warehouse),
		Password: warehouseutils.GetConfigValue(password, pg.Warehouse),
		Port:     warehouseutils.GetConfigValue(port, pg.Warehouse),
		SSLMode:  sslMode,
		SSLDir:   warehouseutils.GetSSLKeyDirPath(pg.Warehouse.Destination.ID),
		timeout:  pg.ConnectTimeout,
		TunnelInfo: warehouseutils.ExtractTunnelInfoFromDestinationConfig(
			pg.Warehouse.Destination.Config,
		),
	}

	return creds
}

func ColumnsWithDataTypes(columns map[string]string, prefix string) string {
	var arr []string
	for name, dataType := range columns {
		arr = append(arr, fmt.Sprintf(`"%s%s" %s`, prefix, name, rudderDataTypesMapToPostgres[dataType]))
	}
	return strings.Join(arr, ",")
}

func (*Postgres) IsEmpty(context.Context, model.Warehouse) (empty bool, err error) {
	return
}

func (pg *Postgres) DownloadLoadFiles(ctx context.Context, tableName string) ([]string, error) {
	objects := pg.Uploader.GetLoadFilesMetadata(ctx, warehouseutils.GetLoadFilesOptions{Table: tableName})
	storageProvider := warehouseutils.ObjectStorageType(pg.Warehouse.Destination.DestinationDefinition.Name, pg.Warehouse.Destination.Config, pg.Uploader.UseRudderStorage())
	downloader, err := filemanager.DefaultFileManagerFactory.New(&filemanager.SettingsT{
		Provider: storageProvider,
		Config: misc.GetObjectStorageConfig(misc.ObjectStorageOptsT{
			Provider:         storageProvider,
			Config:           pg.Warehouse.Destination.Config,
			UseRudderStorage: pg.Uploader.UseRudderStorage(),
			WorkspaceID:      pg.Warehouse.Destination.WorkspaceID,
		}),
	})
	if err != nil {
		pg.logger.Errorf("PG: Error in setting up a downloader for destinationID : %s Error : %v", pg.Warehouse.Destination.ID, err)
		return nil, err
	}
	var fileNames []string
	for _, object := range objects {
		objectName, err := warehouseutils.GetObjectName(object.Location, pg.Warehouse.Destination.Config, pg.ObjectStorage)
		if err != nil {
			pg.logger.Errorf("PG: Error in converting object location to object key for table:%s: %s,%v", tableName, object.Location, err)
			return nil, err
		}
		dirName := fmt.Sprintf(`/%s/`, misc.RudderWarehouseLoadUploadsTmp)
		tmpDirPath, err := misc.CreateTMPDIR()
		if err != nil {
			pg.logger.Errorf("PG: Error in creating tmp directory for downloading load file for table:%s: %s, %v", tableName, object.Location, err)
			return nil, err
		}
		ObjectPath := tmpDirPath + dirName + fmt.Sprintf(`%s_%s_%d/`, pg.Warehouse.Destination.DestinationDefinition.Name, pg.Warehouse.Destination.ID, time.Now().Unix()) + objectName
		err = os.MkdirAll(filepath.Dir(ObjectPath), os.ModePerm)
		if err != nil {
			pg.logger.Errorf("PG: Error in making tmp directory for downloading load file for table:%s: %s, %s %v", tableName, object.Location, err)
			return nil, err
		}
		objectFile, err := os.Create(ObjectPath)
		if err != nil {
			pg.logger.Errorf("PG: Error in creating file in tmp directory for downloading load file for table:%s: %s, %v", tableName, object.Location, err)
			return nil, err
		}
		err = downloader.Download(ctx, objectFile, objectName)
		if err != nil {
			pg.logger.Errorf("PG: Error in downloading file in tmp directory for downloading load file for table:%s: %s, %v", tableName, object.Location, err)
			return nil, err
		}
		fileName := objectFile.Name()
		if err = objectFile.Close(); err != nil {
			pg.logger.Errorf("PG: Error in closing downloaded file in tmp directory for downloading load file for table:%s: %s, %v", tableName, object.Location, err)
			return nil, err
		}
		fileNames = append(fileNames, fileName)
	}
	return fileNames, nil
}

func handleRollbackTimeout(tags stats.Tags) {
	stats.Default.NewTaggedStat("pg_rollback_timeout", stats.CountType, tags).Count(1)
}

func (pg *Postgres) runRollbackWithTimeout(f func() error, onTimeout func(tags stats.Tags), d time.Duration, tags stats.Tags) {
	c := make(chan struct{})
	go func() {
		defer close(c)
		err := f()
		if err != nil {
			pg.logger.Errorf("PG: Error in rolling back transaction : %v", err)
		}
	}()

	select {
	case <-c:
	case <-time.After(d):
		pg.logger.Errorf("PG: Timed out rolling back transaction after %v", d)
		onTimeout(tags)
	}
}

func (pg *Postgres) loadTable(ctx context.Context, tableName string, tableSchemaInUpload model.TableSchema, skipTempTableDelete bool) (stagingTableName string, err error) {
	sqlStatement := fmt.Sprintf(`SET search_path to %q`, pg.Namespace)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	if err != nil {
		return
	}
	pg.logger.Infof("PG: Updated search_path to %s in postgres for PG:%s : %v", pg.Namespace, pg.Warehouse.Destination.ID, sqlStatement)
	pg.logger.Infof("PG: Starting load for table:%s", tableName)

	// tags
	tags := stats.Tags{
		"workspaceId":   pg.Warehouse.WorkspaceID,
		"namepsace":     pg.Namespace,
		"destinationID": pg.Warehouse.Destination.ID,
		"tableName":     tableName,
	}
	// sort column names
	sortedColumnKeys := warehouseutils.SortColumnKeysFromColumnMap(tableSchemaInUpload)

	fileNames, err := pg.DownloadLoadFiles(ctx, tableName)
	defer misc.RemoveFilePaths(fileNames...)
	if err != nil {
		return
	}

	txn, err := pg.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		pg.logger.Errorf("PG: Error while beginning a transaction in db for loading in table:%s: %v", tableName, err)
		return
	}
	// create temporary table
	stagingTableName = warehouseutils.StagingTableName(provider, tableName, tableNameLimit)
	sqlStatement = fmt.Sprintf(`CREATE TABLE "%[1]s".%[2]s (LIKE "%[1]s"."%[3]s")`, pg.Namespace, stagingTableName, tableName)
	pg.logger.Debugf("PG: Creating temporary table for table:%s at %s\n", tableName, sqlStatement)
	_, err = txn.ExecContext(ctx, sqlStatement)
	if err != nil {
		pg.logger.Errorf("PG: Error creating temporary table for table:%s: %v\n", tableName, err)
		tags["stage"] = createStagingTable
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return
	}
	if !skipTempTableDelete {
		defer pg.dropStagingTable(ctx, stagingTableName)
	}

	stmt, err := txn.PrepareContext(ctx, pq.CopyInSchema(pg.Namespace, stagingTableName, sortedColumnKeys...))
	if err != nil {
		pg.logger.Errorf("PG: Error while preparing statement for  transaction in db for loading in staging table:%s: %v\nstmt: %v", stagingTableName, err, stmt)
		tags["stage"] = copyInSchemaStagingTable
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return
	}
	for _, objectFileName := range fileNames {
		var gzipFile *os.File
		gzipFile, err = os.Open(objectFileName)
		if err != nil {
			pg.logger.Errorf("PG: Error opening file using os.Open for file:%s while loading to table %s", objectFileName, tableName)
			tags["stage"] = openLoadFiles
			pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
			return
		}

		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(gzipFile)
		if err != nil {
			pg.logger.Errorf("PG: Error reading file using gzip.NewReader for file:%s while loading to table %s", gzipFile, tableName)
			gzipFile.Close()
			tags["stage"] = readGzipLoadFiles
			pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
			return
		}
		csvReader := csv.NewReader(gzipReader)
		var csvRowsProcessedCount int
		for {
			var record []string
			record, err = csvReader.Read()
			if err != nil {
				if err == io.EOF {
					pg.logger.Debugf("PG: File reading completed while reading csv file for loading in staging table:%s: %s", stagingTableName, objectFileName)
					break
				}
				pg.logger.Errorf("PG: Error while reading csv file %s for loading in staging table:%s: %v", objectFileName, stagingTableName, err)
				tags["stage"] = readCsvLoadFiles
				pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
				return
			}
			if len(sortedColumnKeys) != len(record) {
				err = fmt.Errorf(`load file CSV columns for a row mismatch number found in upload schema. Columns in CSV row: %d, Columns in upload schema of table-%s: %d. Processed rows in csv file until mismatch: %d`, len(record), tableName, len(sortedColumnKeys), csvRowsProcessedCount)
				pg.logger.Error(err)
				tags["stage"] = csvColumnCountMismatch
				pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
				return
			}
			var recordInterface []interface{}
			for _, value := range record {
				if strings.TrimSpace(value) == "" {
					recordInterface = append(recordInterface, nil)
				} else {
					recordInterface = append(recordInterface, value)
				}
			}
			_, err = stmt.ExecContext(ctx, recordInterface...)
			if err != nil {
				pg.logger.Errorf("PG: Error in exec statement for loading in staging table:%s: %v", stagingTableName, err)
				tags["stage"] = loadStagingTable
				pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
				return
			}
			csvRowsProcessedCount++
		}
		_ = gzipReader.Close()
		gzipFile.Close()
	}

	_, err = stmt.ExecContext(ctx)
	if err != nil {
		pg.logger.Errorf("PG: Rollback transaction as there was error while loading staging table:%s: %v", stagingTableName, err)
		tags["stage"] = stagingTableloadStage
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return

	}
	// deduplication process
	primaryKey := "id"
	if column, ok := primaryKeyMap[tableName]; ok {
		primaryKey = column
	}
	partitionKey := "id"
	if column, ok := partitionKeyMap[tableName]; ok {
		partitionKey = column
	}
	var additionalJoinClause string
	if tableName == warehouseutils.DiscardsTable {
		additionalJoinClause = fmt.Sprintf(`AND _source.%[3]s = "%[1]s"."%[2]s"."%[3]s" AND _source.%[4]s = "%[1]s"."%[2]s"."%[4]s"`, pg.Namespace, tableName, "table_name", "column_name")
	}
	sqlStatement = fmt.Sprintf(`DELETE FROM "%[1]s"."%[2]s" USING "%[1]s"."%[3]s" as  _source where (_source.%[4]s = "%[1]s"."%[2]s"."%[4]s" %[5]s)`, pg.Namespace, tableName, stagingTableName, primaryKey, additionalJoinClause)
	pg.logger.Infof("PG: Deduplicate records for table:%s using staging table: %s\n", tableName, sqlStatement)
	err = pg.handleExecContext(ctx, &QueryParams{
		txn:                 txn,
		query:               sqlStatement,
		enableWithQueryPlan: pg.EnableSQLStatementExecutionPlan || slices.Contains(pg.EnableSQLStatementExecutionPlanWorkspaceIDs, pg.Warehouse.WorkspaceID),
	})
	if err != nil {
		pg.logger.Errorf("PG: Error deleting from original table for dedup: %v\n", err)
		tags["stage"] = deleteDedup
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return
	}

	quotedColumnNames := warehouseutils.DoubleQuoteAndJoinByComma(sortedColumnKeys)
	sqlStatement = fmt.Sprintf(`INSERT INTO "%[1]s"."%[2]s" (%[3]s)
									SELECT %[3]s FROM (
										SELECT *, row_number() OVER (PARTITION BY %[5]s ORDER BY received_at DESC) AS _rudder_staging_row_number FROM "%[1]s"."%[4]s"
									) AS _ where _rudder_staging_row_number = 1
									`, pg.Namespace, tableName, quotedColumnNames, stagingTableName, partitionKey)
	pg.logger.Infof("PG: Inserting records for table:%s using staging table: %s\n", tableName, sqlStatement)
	err = pg.handleExecContext(ctx, &QueryParams{
		txn:                 txn,
		query:               sqlStatement,
		enableWithQueryPlan: pg.EnableSQLStatementExecutionPlan || slices.Contains(pg.EnableSQLStatementExecutionPlanWorkspaceIDs, pg.Warehouse.WorkspaceID),
	})

	if err != nil {
		pg.logger.Errorf("PG: Error inserting into original table: %v\n", err)
		tags["stage"] = insertDedup
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return
	}

	if err = txn.Commit(); err != nil {
		pg.logger.Errorf("PG: Error while committing transaction as there was error while loading staging table:%s: %v", stagingTableName, err)
		tags["stage"] = dedupStage
		pg.runRollbackWithTimeout(txn.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		return
	}

	pg.logger.Infof("PG: Complete load for table:%s", tableName)
	return
}

// DeleteBy Need to create a structure with delete parameters instead of simply adding a long list of params
func (pg *Postgres) DeleteBy(ctx context.Context, tableNames []string, params warehouseutils.DeleteByParams) (err error) {
	pg.logger.Infof("PG: Cleaning up the following tables in postgres for PG:%s : %+v", tableNames, params)
	for _, tb := range tableNames {
		sqlStatement := fmt.Sprintf(`DELETE FROM "%[1]s"."%[2]s" WHERE
		context_sources_job_run_id <> $1 AND
		context_sources_task_run_id <> $2 AND
		context_source_id = $3 AND
		received_at < $4`,
			pg.Namespace,
			tb,
		)
		pg.logger.Infof("PG: Deleting rows in table in postgres for PG:%s", pg.Warehouse.Destination.ID)
		pg.logger.Debugf("PG: Executing the statement  %v", sqlStatement)
		if pg.EnableDeleteByJobs {
			_, err = pg.DB.ExecContext(ctx, sqlStatement,
				params.JobRunId,
				params.TaskRunId,
				params.SourceId,
				params.StartTime)
			if err != nil {
				pg.logger.Errorf("Error %s", err)
				return err
			}
		}

	}
	return nil
}

func (pg *Postgres) loadUserTables(ctx context.Context) (errorMap map[string]error) {
	errorMap = map[string]error{warehouseutils.IdentifiesTable: nil}
	sqlStatement := fmt.Sprintf(`SET search_path to %q`, pg.Namespace)
	_, err := pg.DB.ExecContext(ctx, sqlStatement)
	if err != nil {
		errorMap[warehouseutils.IdentifiesTable] = err
		return
	}
	pg.logger.Infof("PG: Updated search_path to %s in postgres for PG:%s : %v", pg.Namespace, pg.Warehouse.Destination.ID, sqlStatement)
	pg.logger.Infof("PG: Starting load for identifies and users tables\n")
	identifyStagingTable, err := pg.loadTable(ctx, warehouseutils.IdentifiesTable, pg.Uploader.GetTableSchemaInUpload(warehouseutils.IdentifiesTable), true)
	defer pg.dropStagingTable(ctx, identifyStagingTable)
	if err != nil {
		errorMap[warehouseutils.IdentifiesTable] = err
		return
	}

	if len(pg.Uploader.GetTableSchemaInUpload(warehouseutils.UsersTable)) == 0 {
		return
	}
	errorMap[warehouseutils.UsersTable] = nil

	if pg.SkipComputingUserLatestTraits || slices.Contains(pg.SkipComputingUserLatestTraitsWorkspaceIDs, pg.Warehouse.WorkspaceID) {
		_, err := pg.loadTable(ctx, warehouseutils.UsersTable, pg.Uploader.GetTableSchemaInUpload(warehouseutils.UsersTable), false)
		if err != nil {
			errorMap[warehouseutils.UsersTable] = err
		}
		return
	}

	unionStagingTableName := warehouseutils.StagingTableName(provider, "users_identifies_union", tableNameLimit)
	stagingTableName := warehouseutils.StagingTableName(provider, warehouseutils.UsersTable, tableNameLimit)
	defer pg.dropStagingTable(ctx, stagingTableName)
	defer pg.dropStagingTable(ctx, unionStagingTableName)

	userColMap := pg.Uploader.GetTableSchemaInWarehouse(warehouseutils.UsersTable)
	var userColNames, firstValProps []string
	for colName := range userColMap {
		if colName == "id" {
			continue
		}
		userColNames = append(userColNames, fmt.Sprintf(`%q`, colName))
		caseSubQuery := fmt.Sprintf(`case
						  when (select true) then (
						  	select "%[1]s" from "%[3]s"."%[2]s" as staging_table
						  	where x.id = staging_table.id
							  and "%[1]s" is not null
							  order by received_at desc
						  	limit 1)
						  end as "%[1]s"`, colName, unionStagingTableName, pg.Namespace)
		firstValProps = append(firstValProps, caseSubQuery)
	}

	sqlStatement = fmt.Sprintf(`CREATE TABLE "%[1]s".%[5]s as (
												(
													SELECT id, %[4]s FROM "%[1]s"."%[2]s" WHERE id in (SELECT user_id FROM "%[1]s"."%[3]s" WHERE user_id IS NOT NULL)
												) UNION
												(
													SELECT user_id, %[4]s FROM "%[1]s"."%[3]s"  WHERE user_id IS NOT NULL
												)
											)`, pg.Namespace, warehouseutils.UsersTable, identifyStagingTable, strings.Join(userColNames, ","), unionStagingTableName)

	pg.logger.Infof("PG: Creating staging table for union of users table with identify staging table: %s\n", sqlStatement)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	if err != nil {
		errorMap[warehouseutils.UsersTable] = err
		return
	}

	sqlStatement = fmt.Sprintf(`CREATE TABLE %[4]s.%[1]s AS (SELECT DISTINCT * FROM
										(
											SELECT
											x.id, %[2]s
											FROM %[4]s.%[3]s as x
										) as xyz
									)`,
		stagingTableName,
		strings.Join(firstValProps, ","),
		unionStagingTableName,
		pg.Namespace,
	)

	pg.logger.Debugf("PG: Creating staging table for users: %s\n", sqlStatement)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	if err != nil {
		errorMap[warehouseutils.UsersTable] = err
		return
	}

	// BEGIN TRANSACTION
	tx, err := pg.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		errorMap[warehouseutils.UsersTable] = err
		return
	}

	primaryKey := "id"
	sqlStatement = fmt.Sprintf(`DELETE FROM "%[1]s"."%[2]s" using "%[1]s"."%[3]s" _source where (_source.%[4]s = %[1]s.%[2]s.%[4]s)`, pg.Namespace, warehouseutils.UsersTable, stagingTableName, primaryKey)
	pg.logger.Infof("PG: Dedup records for table:%s using staging table: %s\n", warehouseutils.UsersTable, sqlStatement)
	// tags
	tags := stats.Tags{
		"workspaceId": pg.Warehouse.WorkspaceID,
		"destId":      pg.Warehouse.Destination.ID,
		"tableName":   warehouseutils.UsersTable,
	}
	err = pg.handleExecContext(ctx, &QueryParams{
		txn:                 tx,
		query:               sqlStatement,
		enableWithQueryPlan: pg.EnableSQLStatementExecutionPlan || slices.Contains(pg.EnableSQLStatementExecutionPlanWorkspaceIDs, pg.Warehouse.WorkspaceID),
	})
	if err != nil {
		pg.logger.Errorf("PG: Error deleting from original table for dedup: %v\n", err)
		tags["stage"] = deleteDedup
		pg.runRollbackWithTimeout(tx.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		errorMap[warehouseutils.UsersTable] = err
		return
	}

	sqlStatement = fmt.Sprintf(`INSERT INTO "%[1]s"."%[2]s" (%[4]s) SELECT %[4]s FROM  "%[1]s"."%[3]s"`, pg.Namespace, warehouseutils.UsersTable, stagingTableName, strings.Join(append([]string{"id"}, userColNames...), ","))
	pg.logger.Infof("PG: Inserting records for table:%s using staging table: %s\n", warehouseutils.UsersTable, sqlStatement)
	err = pg.handleExecContext(ctx, &QueryParams{
		txn:                 tx,
		query:               sqlStatement,
		enableWithQueryPlan: pg.EnableSQLStatementExecutionPlan || slices.Contains(pg.EnableSQLStatementExecutionPlanWorkspaceIDs, pg.Warehouse.WorkspaceID),
	})

	if err != nil {
		pg.logger.Errorf("PG: Error inserting into users table from staging table: %v\n", err)
		tags["stage"] = insertDedup
		pg.runRollbackWithTimeout(tx.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		errorMap[warehouseutils.UsersTable] = err
		return
	}

	err = tx.Commit()
	if err != nil {
		pg.logger.Errorf("PG: Error in transaction commit for users table: %v\n", err)
		tags["stage"] = dedupStage
		pg.runRollbackWithTimeout(tx.Rollback, handleRollbackTimeout, pg.TxnRollbackTimeout, tags)
		errorMap[warehouseutils.UsersTable] = err
		return
	}
	return
}

func (pg *Postgres) schemaExists(ctx context.Context) (exists bool, err error) {
	sqlStatement := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = '%s');`, pg.Namespace)
	err = pg.DB.QueryRowContext(ctx, sqlStatement).Scan(&exists)
	return
}

func (pg *Postgres) CreateSchema(ctx context.Context) (err error) {
	var schemaExists bool
	schemaExists, err = pg.schemaExists(ctx)
	if err != nil {
		pg.logger.Errorf("PG: Error checking if schema: %s exists: %v", pg.Namespace, err)
		return err
	}
	if schemaExists {
		pg.logger.Infof("PG: Skipping creating schema: %s since it already exists", pg.Namespace)
		return
	}
	sqlStatement := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, pg.Namespace)
	pg.logger.Infof("PG: Creating schema name in postgres for PG:%s : %v", pg.Warehouse.Destination.ID, sqlStatement)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	return
}

func (pg *Postgres) dropStagingTable(ctx context.Context, stagingTableName string) {
	pg.logger.Infof("PG: dropping table %+v\n", stagingTableName)
	_, err := pg.DB.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS "%[1]s"."%[2]s"`, pg.Namespace, stagingTableName))
	if err != nil {
		pg.logger.Errorf("PG:  Error dropping staging table %s in postgres: %v", stagingTableName, err)
	}
}

func (pg *Postgres) createTable(ctx context.Context, name string, columns model.TableSchema) (err error) {
	sqlStatement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%[1]s"."%[2]s" ( %v )`, pg.Namespace, name, ColumnsWithDataTypes(columns, ""))
	pg.logger.Infof("PG: Creating table in postgres for PG:%s : %v", pg.Warehouse.Destination.ID, sqlStatement)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	return
}

func (pg *Postgres) CreateTable(ctx context.Context, tableName string, columnMap model.TableSchema) (err error) {
	// set the schema in search path. so that we can query table with unqualified name which is just the table name rather than using schema.table in queries
	sqlStatement := fmt.Sprintf(`SET search_path to %q`, pg.Namespace)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	if err != nil {
		return err
	}
	pg.logger.Infof("PG: Updated search_path to %s in postgres for PG:%s : %v", pg.Namespace, pg.Warehouse.Destination.ID, sqlStatement)
	err = pg.createTable(ctx, tableName, columnMap)
	return err
}

func (pg *Postgres) DropTable(ctx context.Context, tableName string) (err error) {
	sqlStatement := `DROP TABLE "%[1]s"."%[2]s"`
	pg.logger.Infof("PG: Dropping table in postgres for PG:%s : %v", pg.Warehouse.Destination.ID, sqlStatement)
	_, err = pg.DB.ExecContext(ctx, fmt.Sprintf(sqlStatement, pg.Namespace, tableName))
	return
}

func (pg *Postgres) AddColumns(ctx context.Context, tableName string, columnsInfo []warehouseutils.ColumnInfo) (err error) {
	var (
		query        string
		queryBuilder strings.Builder
	)

	// set the schema in search path. so that we can query table with unqualified name which is just the table name rather than using schema.table in queries
	query = fmt.Sprintf(`SET search_path to %q`, pg.Namespace)
	if _, err = pg.DB.ExecContext(ctx, query); err != nil {
		return
	}
	pg.logger.Infof("PG: Updated search_path to %s in postgres for PG:%s : %v", pg.Namespace, pg.Warehouse.Destination.ID, query)

	queryBuilder.WriteString(fmt.Sprintf(`
		ALTER TABLE
		  %s.%s`,
		pg.Namespace,
		tableName,
	))

	for _, columnInfo := range columnsInfo {
		queryBuilder.WriteString(fmt.Sprintf(` ADD COLUMN IF NOT EXISTS %q %s,`, columnInfo.Name, rudderDataTypesMapToPostgres[columnInfo.Type]))
	}

	query = strings.TrimSuffix(queryBuilder.String(), ",")
	query += ";"

	pg.logger.Infof("PG: Adding columns for destinationID: %s, tableName: %s with query: %v", pg.Warehouse.Destination.ID, tableName, query)
	_, err = pg.DB.ExecContext(ctx, query)
	return
}

func (*Postgres) AlterColumn(context.Context, string, string, string) (model.AlterTableResponse, error) {
	return model.AlterTableResponse{}, nil
}

func (pg *Postgres) TestConnection(ctx context.Context, warehouse model.Warehouse) error {
	if warehouse.Destination.Config["sslMode"] == verifyCA {
		if sslKeyError := warehouseutils.WriteSSLKeys(warehouse.Destination); sslKeyError.IsError() {
			return fmt.Errorf("writing ssl keys: %s", sslKeyError.Error())
		}
	}

	err := pg.DB.PingContext(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("connection timeout: %w", err)
	}
	if err != nil {
		return fmt.Errorf("pinging: %w", err)
	}

	return nil
}

func (pg *Postgres) Setup(_ context.Context, warehouse model.Warehouse, uploader warehouseutils.Uploader) (err error) {
	pg.Warehouse = warehouse
	pg.Namespace = warehouse.Namespace
	pg.Uploader = uploader
	pg.ObjectStorage = warehouseutils.ObjectStorageType(warehouseutils.POSTGRES, warehouse.Destination.Config, pg.Uploader.UseRudderStorage())

	pg.DB, err = pg.connect()
	return err
}

func (pg *Postgres) CrashRecover(ctx context.Context) {
	pg.dropDanglingStagingTables(ctx)
}

func (pg *Postgres) dropDanglingStagingTables(ctx context.Context) bool {
	sqlStatement := `
			SELECT
			  table_name
			FROM
			  information_schema.tables
			WHERE
			  table_schema = $1 AND
			  table_name like $2;
	`
	rows, err := pg.DB.QueryContext(ctx,
		sqlStatement,
		pg.Namespace,
		fmt.Sprintf(`%s%%`, warehouseutils.StagingTablePrefix(provider)),
	)
	if err != nil {
		pg.logger.Errorf("WH: PG: Error dropping dangling staging tables in PG: %v\nQuery: %s\n", err, sqlStatement)
		return false
	}
	defer func() { _ = rows.Close() }()

	var stagingTableNames []string
	for rows.Next() {
		var tableName string
		err := rows.Scan(&tableName)
		if err != nil {
			panic(fmt.Errorf("scan result from query: %s\nwith Error : %w", sqlStatement, err))
		}
		stagingTableNames = append(stagingTableNames, tableName)
	}
	if err := rows.Err(); err != nil {
		panic(fmt.Errorf("iterate result from query: %s\nwith Error : %w", sqlStatement, err))
	}
	pg.logger.Infof("WH: PG: Dropping dangling staging tables: %+v  %+v\n", len(stagingTableNames), stagingTableNames)
	delSuccess := true
	for _, stagingTableName := range stagingTableNames {
		_, err := pg.DB.ExecContext(ctx, fmt.Sprintf(`DROP TABLE "%[1]s"."%[2]s"`, pg.Namespace, stagingTableName))
		if err != nil {
			pg.logger.Errorf("WH: PG:  Error dropping dangling staging table: %s in PG: %v\n", stagingTableName, err)
			delSuccess = false
		}
	}
	return delSuccess
}

// FetchSchema queries postgres and returns the schema associated with provided namespace
func (pg *Postgres) FetchSchema(ctx context.Context) (model.Schema, model.Schema, error) {
	schema := make(model.Schema)
	unrecognizedSchema := make(model.Schema)

	sqlStatement := `
		SELECT
		  table_name,
		  column_name,
		  data_type
		FROM
		  INFORMATION_SCHEMA.COLUMNS
		WHERE
		  table_schema = $1
		  AND table_name NOT LIKE $2;
	`
	rows, err := pg.DB.QueryContext(
		ctx,
		sqlStatement,
		pg.Namespace,
		fmt.Sprintf(`%s%%`, warehouseutils.StagingTablePrefix(provider)),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return schema, unrecognizedSchema, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fetching schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var tableName, columnName, columnType string

		if err := rows.Scan(&tableName, &columnName, &columnType); err != nil {
			return nil, nil, fmt.Errorf("scanning schema: %w", err)
		}

		if _, ok := schema[tableName]; !ok {
			schema[tableName] = make(model.TableSchema)
		}
		if datatype, ok := postgresDataTypesMapToRudder[columnType]; ok {
			schema[tableName][columnName] = datatype
		} else {
			if _, ok := unrecognizedSchema[tableName]; !ok {
				unrecognizedSchema[tableName] = make(model.TableSchema)
			}
			unrecognizedSchema[tableName][columnName] = warehouseutils.MISSING_DATATYPE

			warehouseutils.WHCounterStat(warehouseutils.RUDDER_MISSING_DATATYPE, &pg.Warehouse, warehouseutils.Tag{Name: "datatype", Value: columnType}).Count(1)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("fetching schema: %w", err)
	}

	return schema, unrecognizedSchema, nil
}

func (pg *Postgres) LoadUserTables(ctx context.Context) map[string]error {
	return pg.loadUserTables(ctx)
}

func (pg *Postgres) LoadTable(ctx context.Context, tableName string) error {
	_, err := pg.loadTable(ctx, tableName, pg.Uploader.GetTableSchemaInUpload(tableName), false)
	return err
}

func (pg *Postgres) Cleanup(ctx context.Context) {
	if pg.DB != nil {
		pg.dropDanglingStagingTables(ctx)
		_ = pg.DB.Close()
	}
}

func (*Postgres) LoadIdentityMergeRulesTable(context.Context) (err error) {
	return
}

func (*Postgres) LoadIdentityMappingsTable(context.Context) (err error) {
	return
}

func (*Postgres) DownloadIdentityRules(context.Context, *misc.GZipWriter) (err error) {
	return
}

func (pg *Postgres) GetTotalCountInTable(ctx context.Context, tableName string) (int64, error) {
	var (
		total        int64
		err          error
		sqlStatement string
	)
	sqlStatement = fmt.Sprintf(`
		SELECT count(*) FROM "%[1]s"."%[2]s";
	`,
		pg.Namespace,
		tableName,
	)
	err = pg.DB.QueryRowContext(ctx, sqlStatement).Scan(&total)
	return total, err
}

func (pg *Postgres) Connect(_ context.Context, warehouse model.Warehouse) (client.Client, error) {
	if warehouse.Destination.Config["sslMode"] == "verify-ca" {
		if err := warehouseutils.WriteSSLKeys(warehouse.Destination); err.IsError() {
			pg.logger.Error(err.Error())
			return client.Client{}, fmt.Errorf(err.Error())
		}
	}
	pg.Warehouse = warehouse
	pg.Namespace = warehouse.Namespace
	pg.ObjectStorage = warehouseutils.ObjectStorageType(
		warehouseutils.POSTGRES,
		warehouse.Destination.Config,
		misc.IsConfiguredToUseRudderObjectStorage(pg.Warehouse.Destination.Config),
	)
	dbHandle, err := pg.connect()
	if err != nil {
		return client.Client{}, err
	}

	return client.Client{Type: client.SQLClient, SQL: dbHandle.DB}, err
}

func (pg *Postgres) LoadTestTable(ctx context.Context, _, tableName string, payloadMap map[string]interface{}, _ string) (err error) {
	sqlStatement := fmt.Sprintf(`INSERT INTO %q.%q (%v) VALUES (%s)`,
		pg.Namespace,
		tableName,
		fmt.Sprintf(`%q, %q`, "id", "val"),
		fmt.Sprintf(`'%d', '%s'`, payloadMap["id"], payloadMap["val"]),
	)
	_, err = pg.DB.ExecContext(ctx, sqlStatement)
	return
}

func (pg *Postgres) SetConnectionTimeout(timeout time.Duration) {
	pg.ConnectTimeout = timeout
}

type QueryParams struct {
	txn                 *sqlmiddleware.Tx
	db                  *sqlmiddleware.DB
	query               string
	enableWithQueryPlan bool
}

func (q *QueryParams) validate() (err error) {
	if q.txn == nil && q.db == nil {
		return fmt.Errorf("both txn and db are nil")
	}
	return
}

// handleExec
// Print execution plan if enableWithQueryPlan is set to true else return result set.
// Currently, these statements are supported by EXPLAIN
// Any INSERT, UPDATE, DELETE whose execution plan you wish to see.
func (pg *Postgres) handleExecContext(ctx context.Context, e *QueryParams) (err error) {
	sqlStatement := e.query

	if err = e.validate(); err != nil {
		err = fmt.Errorf("[WH][POSTGRES] Not able to handle query execution for statement: %s as both txn and db are nil", sqlStatement)
		return
	}

	if e.enableWithQueryPlan {
		sqlStatement := "EXPLAIN " + e.query

		var rows *sql.Rows
		if e.txn != nil {
			rows, err = e.txn.QueryContext(ctx, sqlStatement)
		} else if e.db != nil {
			rows, err = e.db.QueryContext(ctx, sqlStatement)
		}
		if err != nil {
			err = fmt.Errorf("[WH][POSTGRES] error occurred while handling transaction for query: %s with err: %w", sqlStatement, err)
			return
		}
		defer func() { _ = rows.Close() }()

		var response []string
		for rows.Next() {
			var s string
			if err = rows.Scan(&s); err != nil {
				err = fmt.Errorf("[WH][POSTGRES] Error occurred while processing destination revisionID query %+v with err: %w", e, err)
				return
			}
			response = append(response, s)
		}
		if err = rows.Err(); err != nil {
			err = fmt.Errorf("[WH][POSTGRES] Error occurred while processing destination revisionID query %+v with err: %w", e, err)
			return
		}
		pg.logger.Infof(fmt.Sprintf(`[WH][POSTGRES] Execution Query plan for statement: %s is %s`, sqlStatement, strings.Join(response, `
`)))
	}
	if e.txn != nil {
		_, err = e.txn.ExecContext(ctx, sqlStatement)
	} else if e.db != nil {
		_, err = e.db.ExecContext(ctx, sqlStatement)
	}
	return
}

func (*Postgres) ErrorMappings() []model.JobError {
	return errorsMappings
}
