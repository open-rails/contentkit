package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	contentkit "github.com/open-rails/contentkit"
)

var signalName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// An optional integration lane borrows a ClickHouse server but owns only the
// uniquely named database it successfully creates here.
func prepareSignals(ctx context.Context, c config, cfg *contentkit.Config) (func(context.Context) error, error) {
	if c.signalAddr == "" {
		return nil, nil
	}
	if !strings.HasPrefix(c.signalDatabase, "ckui_e2e_") || !signalName.MatchString(c.signalDatabase) {
		return nil, fmt.Errorf("signal fixture database must be a unique ckui_e2e_ name")
	}
	if c.signalCluster != "" && !signalName.MatchString(c.signalCluster) {
		return nil, fmt.Errorf("invalid signal fixture cluster")
	}
	admin, err := clickhouse.Open(&clickhouse.Options{Addr: []string{c.signalAddr}, Auth: clickhouse.Auth{
		Database: "default", Username: c.signalUser, Password: c.signalPassword}, DialTimeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	cluster := ""
	if c.signalCluster != "" {
		cluster = " ON CLUSTER `" + c.signalCluster + "`"
	}
	database := "`" + c.signalDatabase + "`"
	// No IF NOT EXISTS: an existing database is never adopted or later dropped.
	if err := admin.Exec(ctx, "CREATE DATABASE "+database+cluster); err != nil {
		_ = admin.Close()
		return nil, err
	}
	cfg.MeasuredEngagement = true // isolated fixture owns all writers and erasure executors
	cfg.ClickHouse = contentkit.ClickHouseConfig{Addr: c.signalAddr, Username: c.signalUser, Password: c.signalPassword,
		Database: c.signalDatabase, Cluster: c.signalCluster}
	return func(ctx context.Context) error {
		defer admin.Close()
		return admin.Exec(ctx, "DROP DATABASE "+database+cluster+" SYNC")
	}, nil
}
