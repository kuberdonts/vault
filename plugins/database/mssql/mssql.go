package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/hashicorp/errwrap"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/database/helper/connutil"
	"github.com/hashicorp/vault/sdk/database/helper/credsutil"
	"github.com/hashicorp/vault/sdk/database/helper/dbutil"
	"github.com/hashicorp/vault/sdk/database/newdbplugin"
	"github.com/hashicorp/vault/sdk/helper/dbtxn"
	"github.com/hashicorp/vault/sdk/helper/strutil"
)

const msSQLTypeName = "mssql"

var _ newdbplugin.Database = &MSSQL{}

// MSSQL is an implementation of Database interface
type MSSQL struct {
	*connutil.SQLConnectionProducer
}

func New() (interface{}, error) {
	db := new()
	// Wrap the plugin with middleware to sanitize errors
	dbType := newdbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues)

	return dbType, nil
}

func new() *MSSQL {
	connProducer := &connutil.SQLConnectionProducer{}
	connProducer.Type = msSQLTypeName

	return &MSSQL{
		SQLConnectionProducer: connProducer,
	}
}

// Run instantiates a MSSQL object, and runs the RPC server for the plugin
func Run(apiTLSConfig *api.TLSConfig) error {
	dbType, err := New()
	if err != nil {
		return err
	}

	newdbplugin.Serve(dbType.(newdbplugin.Database), api.VaultPluginTLSProvider(apiTLSConfig))

	return nil
}

// Type returns the TypeName for this backend
func (m *MSSQL) Type() (string, error) {
	return msSQLTypeName, nil
}

func (m *MSSQL) secretValues() map[string]string {
	return map[string]string{
		m.Password: "[password]",
	}
}

func (m *MSSQL) getConnection(ctx context.Context) (*sql.DB, error) {
	db, err := m.Connection(ctx)
	if err != nil {
		return nil, err
	}

	return db.(*sql.DB), nil
}

func (m *MSSQL) Initialize(ctx context.Context, req newdbplugin.InitializeRequest) (newdbplugin.InitializeResponse, error) {
	newConf, err := m.SQLConnectionProducer.Init(ctx, req.Config, req.VerifyConnection)
	if err != nil {
		return newdbplugin.InitializeResponse{}, err
	}
	resp := newdbplugin.InitializeResponse{
		Config: newConf,
	}
	return resp, nil
}

// NewUser generates the username/password on the underlying MSSQL secret backend as instructed by
// the statements provided.
func (m *MSSQL) NewUser(ctx context.Context, req newdbplugin.NewUserRequest) (newdbplugin.NewUserResponse, error) {
	m.Lock()
	defer m.Unlock()

	db, err := m.getConnection(ctx)
	if err != nil {
		return newdbplugin.NewUserResponse{}, fmt.Errorf("unable to get connection: %w", err)
	}

	if len(req.Statements.Commands) == 0 {
		return newdbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	username, err := credsutil.GenerateUsername(
		credsutil.DisplayName(req.UsernameConfig.DisplayName, 20),
		credsutil.RoleName(req.UsernameConfig.RoleName, 20),
		credsutil.MaxLength(128),
		credsutil.Separator("-"),
	)
	if err != nil {
		return newdbplugin.NewUserResponse{}, err
	}

	expirationStr := req.Expiration.Format("2006-01-02 15:04:05-0700")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return newdbplugin.NewUserResponse{}, err
	}
	defer tx.Rollback()

	for _, stmt := range req.Statements.Commands {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name":       username,
				"password":   req.Password,
				"expiration": expirationStr,
			}

			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return newdbplugin.NewUserResponse{}, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return newdbplugin.NewUserResponse{}, err
	}

	resp := newdbplugin.NewUserResponse{
		Username: username,
	}

	return resp, nil
}

// DeleteUser attempts to drop the specified user. It will first attempt to disable login,
// then kill pending connections from that user, and finally drop the user and login from the
// database instance.
func (m *MSSQL) DeleteUser(ctx context.Context, req newdbplugin.DeleteUserRequest) (newdbplugin.DeleteUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		err := m.revokeUserDefault(ctx, req.Username)
		return newdbplugin.DeleteUserResponse{}, err
	}

	db, err := m.getConnection(ctx)
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, fmt.Errorf("unable to get connection: %w", err)
	}

	merr := &multierror.Error{}

	// Execute each query
	for _, stmt := range req.Statements.Commands {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name": req.Username,
			}
			if err := dbtxn.ExecuteDBQuery(ctx, db, m, query); err != nil {
				merr = multierror.Append(merr, err)
			}
		}
	}

	return newdbplugin.DeleteUserResponse{}, merr.ErrorOrNil()
}

func (m *MSSQL) revokeUserDefault(ctx context.Context, username string) error {
	// Get connection
	db, err := m.getConnection(ctx)
	if err != nil {
		return err
	}

	// First disable server login
	disableStmt, err := db.PrepareContext(ctx, fmt.Sprintf("ALTER LOGIN [%s] DISABLE;", username))
	if err != nil {
		return err
	}
	defer disableStmt.Close()
	if _, err := disableStmt.ExecContext(ctx); err != nil {
		return err
	}

	// Query for sessions for the login so that we can kill any outstanding
	// sessions.  There cannot be any active sessions before we drop the logins
	// This isn't done in a transaction because even if we fail along the way,
	// we want to remove as much access as possible
	sessionStmt, err := db.PrepareContext(ctx,
		"SELECT session_id FROM sys.dm_exec_sessions WHERE login_name = @p1;")
	if err != nil {
		return err
	}
	defer sessionStmt.Close()

	sessionRows, err := sessionStmt.QueryContext(ctx, username)
	if err != nil {
		return err
	}
	defer sessionRows.Close()

	var revokeStmts []string
	for sessionRows.Next() {
		var sessionID int
		err = sessionRows.Scan(&sessionID)
		if err != nil {
			return err
		}
		revokeStmts = append(revokeStmts, fmt.Sprintf("KILL %d;", sessionID))
	}

	// Query for database users using undocumented stored procedure for now since
	// it is the easiest way to get this information;
	// we need to drop the database users before we can drop the login and the role
	// This isn't done in a transaction because even if we fail along the way,
	// we want to remove as much access as possible
	stmt, err := db.PrepareContext(ctx, "EXEC master.dbo.sp_msloginmappings @p1;")
	if err != nil {
		return err
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx, username)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var loginName, dbName, qUsername, aliasName sql.NullString
		err = rows.Scan(&loginName, &dbName, &qUsername, &aliasName)
		if err != nil {
			return err
		}
		if !dbName.Valid {
			continue
		}
		revokeStmts = append(revokeStmts, fmt.Sprintf(dropUserSQL, dbName.String, username, username))
	}

	// we do not stop on error, as we want to remove as
	// many permissions as possible right now
	var lastStmtError error
	for _, query := range revokeStmts {
		if err := dbtxn.ExecuteDBQuery(ctx, db, nil, query); err != nil {
			lastStmtError = err
		}
	}

	// can't drop if not all database users are dropped
	if rows.Err() != nil {
		return errwrap.Wrapf("could not generate sql statements for all rows: {{err}}", rows.Err())
	}
	if lastStmtError != nil {
		return errwrap.Wrapf("could not perform all sql statements: {{err}}", lastStmtError)
	}

	// Drop this login
	stmt, err = db.PrepareContext(ctx, fmt.Sprintf(dropLoginSQL, username, username))
	if err != nil {
		return err
	}
	defer stmt.Close()
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}

	return nil
}

func (m *MSSQL) UpdateUser(ctx context.Context, req newdbplugin.UpdateUserRequest) (newdbplugin.UpdateUserResponse, error) {
	if req.Password == nil && req.Expiration == nil {
		return newdbplugin.UpdateUserResponse{}, fmt.Errorf("no changes requested")
	}
	if req.Password != nil {
		err := m.updateUserPass(ctx, req.Username, req.Password)
		return newdbplugin.UpdateUserResponse{}, err
	}
	// Expiration is a no-op
	return newdbplugin.UpdateUserResponse{}, nil
}

func (m *MSSQL) updateUserPass(ctx context.Context, username string, changePass *newdbplugin.ChangePassword) error {
	stmts := changePass.Statements.Commands
	if len(stmts) == 0 {
		stmts = []string{alterLoginSQL}
	}

	password := changePass.NewPassword

	if username == "" || password == "" {
		return errors.New("must provide both username and password")
	}

	m.Lock()
	defer m.Unlock()

	db, err := m.getConnection(ctx)
	if err != nil {
		return err
	}

	var exists bool

	err = db.QueryRowContext(ctx, "SELECT 1 FROM master.sys.server_principals where name = N'$1'", username).Scan(&exists)

	if err != nil && err != sql.ErrNoRows {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	for _, stmt := range stmts {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name":     username,
				"username": username,
				"password": password,
			}
			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return fmt.Errorf("failed to execute query: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

const dropUserSQL = `
USE [%s]
IF EXISTS
  (SELECT name
   FROM sys.database_principals
   WHERE name = N'%s')
BEGIN
  DROP USER [%s]
END
`

const dropLoginSQL = `
IF EXISTS
  (SELECT name
   FROM master.sys.server_principals
   WHERE name = N'%s')
BEGIN
  DROP LOGIN [%s]
END
`
const alterLoginSQL = `
ALTER LOGIN [{{username}}] WITH PASSWORD = '{{password}}' 
`
