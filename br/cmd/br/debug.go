// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"reflect"

	"github.com/gogo/protobuf/proto"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/conn"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/mock/mockid"
	restoreutils "github.com/pingcap/tidb/br/pkg/restore/utils"
	"github.com/pingcap/tidb/br/pkg/rtree"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/task"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/br/pkg/version/build"
	"github.com/pingcap/tidb/pkg/meta/model"
	tidblogutil "github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// NewDebugCommand return a debug subcommand.
func NewDebugCommand() *cobra.Command {
	meta := &cobra.Command{
		Use:          "debug <subcommand>",
		Short:        "commands to check/debug backup data",
		SilenceUsage: false,
		PersistentPreRunE: func(c *cobra.Command, args []string) error {
			if err := Init(c); err != nil {
				return errors.Trace(err)
			}
			build.LogInfo(build.BR)
			tidblogutil.LogEnvVariables()
			task.LogArguments(c)
			return nil
		},
		// To be compatible with older BR.
		Aliases: []string{"validate"},
	}
	meta.AddCommand(newCheckSumCommand())
	meta.AddCommand(newBackupMetaCommand())
	meta.AddCommand(decodeBackupMetaCommand())
	meta.AddCommand(encodeBackupMetaCommand())
	meta.AddCommand(setPDConfigCommand())
	meta.AddCommand(searchStreamBackupCommand())
	meta.Hidden = true

	return meta
}

func newCheckSumCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "checksum",
		Short: "check the backup data",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			var cfg task.Config
			if err := cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}

			_, s, backupMeta, err := task.ReadBackupMeta(ctx, metautil.MetaFile, &cfg)
			if err != nil {
				return errors.Trace(err)
			}

			reader := metautil.NewMetaReader(backupMeta, s, &cfg.CipherInfo)
			dbs, err := metautil.LoadBackupTables(ctx, reader, false)
			if err != nil {
				return errors.Trace(err)
			}

			for _, db := range dbs {
				for _, tbl := range db.Tables {
					var calCRC64 uint64
					var totalKVs uint64
					var totalBytes uint64
					for _, files := range tbl.FilesOfPhysicals {
						for _, file := range files {
							calCRC64 ^= file.Crc64Xor
							totalKVs += file.GetTotalKvs()
							totalBytes += file.GetTotalBytes()
							log.Info("file info", zap.Stringer("table", tbl.Info.Name),
								zap.String("file", file.GetName()),
								zap.Uint64("crc64xor", file.GetCrc64Xor()),
								zap.Uint64("totalKvs", file.GetTotalKvs()),
								zap.Uint64("totalBytes", file.GetTotalBytes()),
								zap.Uint64("startVersion", file.GetStartVersion()),
								zap.Uint64("endVersion", file.GetEndVersion()),
								logutil.Key("startKey", file.GetStartKey()),
								logutil.Key("endKey", file.GetEndKey()),
							)

							var data []byte
							data, err = s.ReadFile(ctx, file.Name)
							if err != nil {
								return errors.Trace(err)
							}
							s := sha256.Sum256(data)
							if !bytes.Equal(s[:], file.Sha256) {
								return errors.Annotatef(berrors.ErrBackupChecksumMismatch, `
backup data checksum failed: %s may be changed
calculated sha256 is %s,
origin sha256 is %s`,
									file.Name, hex.EncodeToString(s[:]), hex.EncodeToString(file.Sha256))
							}
						}
					}
					if tbl.Info == nil {
						log.Info("table info(empty)", zap.Stringer("db", db.Info.Name))
					} else {
						log.Info("table info", zap.Stringer("table", tbl.Info.Name),
							zap.Uint64("CRC64", calCRC64),
							zap.Uint64("totalKvs", totalKVs),
							zap.Uint64("totalBytes", totalBytes),
							zap.Uint64("schemaTotalKvs", tbl.TotalKvs),
							zap.Uint64("schemaTotalBytes", tbl.TotalBytes),
							zap.Uint64("schemaCRC64", tbl.Crc64Xor))
					}
				}
			}
			cmd.Println("backup data checksum succeed!")
			return nil
		},
	}
	command.Hidden = true
	return command
}

func newBackupMetaCommand() *cobra.Command {
	command := &cobra.Command{
		Use:          "backupmeta",
		Short:        "utilities of backupmeta",
		SilenceUsage: false,
	}
	command.AddCommand(newBackupMetaValidateCommand())
	return command
}

func newBackupMetaValidateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "validate",
		Short: "validate key range and rewrite rules of backupmeta",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			tableIDOffset, err := cmd.Flags().GetUint64("offset")
			if err != nil {
				return errors.Trace(err)
			}

			var cfg task.Config
			if err = cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}
			_, s, backupMeta, err := task.ReadBackupMeta(ctx, metautil.MetaFile, &cfg)
			if err != nil {
				log.Error("read backupmeta failed", zap.Error(err))
				return errors.Trace(err)
			}
			reader := metautil.NewMetaReader(backupMeta, s, &cfg.CipherInfo)
			dbs, err := metautil.LoadBackupTables(ctx, reader, false)
			if err != nil {
				log.Error("load tables failed", zap.Error(err))
				return errors.Trace(err)
			}
			files := make([]*backuppb.File, 0)
			tables := make([]*metautil.Table, 0)
			for _, db := range dbs {
				for _, table := range db.Tables {
					for _, fs := range table.FilesOfPhysicals {
						files = append(files, fs...)
					}
				}
				tables = append(tables, db.Tables...)
			}
			// Check if the ranges of files overlapped
			rangeTree := rtree.NewRangeTree()
			for _, file := range files {
				if out := rangeTree.InsertRange(rtree.Range{
					KeyRange: rtree.KeyRange{
						StartKey: file.GetStartKey(),
						EndKey:   file.GetEndKey(),
					},
				}); out != nil {
					log.Error(
						"file ranges overlapped",
						zap.Stringer("out", out),
						logutil.File(file),
					)
				}
			}

			tableIDAllocator := mockid.NewIDAllocator()
			// Advance table ID allocator to the offset.
			for offset := uint64(0); offset < tableIDOffset; offset++ {
				_, _ = tableIDAllocator.Alloc() // Ignore error
			}
			rewriteRules := &restoreutils.RewriteRules{
				Data: make([]*import_sstpb.RewriteRule, 0),
			}
			tableIDMap := make(map[int64]int64)
			// Simulate to create table
			for _, table := range tables {
				if table.Info == nil {
					// empty database.
					continue
				}
				indexIDAllocator := mockid.NewIDAllocator()
				newTable := new(model.TableInfo)
				tableID, _ := tableIDAllocator.Alloc()
				newTable.ID = int64(tableID)
				newTable.Name = table.Info.Name
				newTable.Indices = make([]*model.IndexInfo, len(table.Info.Indices))
				for i, indexInfo := range table.Info.Indices {
					indexID, _ := indexIDAllocator.Alloc()
					newTable.Indices[i] = &model.IndexInfo{
						ID:   int64(indexID),
						Name: indexInfo.Name,
					}
				}
				if table.Info.Partition != nil {
					if table.Info.Partition != nil {
						newTable.Partition = &model.PartitionInfo{
							Definitions: make([]model.PartitionDefinition, len(table.Info.Partition.Definitions)),
						}
					}
					for _, old := range table.Info.Partition.Definitions {
						partitionID, _ := tableIDAllocator.Alloc()
						newTable.Partition.Definitions = append(newTable.Partition.Definitions, model.PartitionDefinition{
							ID:   int64(partitionID),
							Name: old.Name,
						})
					}
				}

				rules := restoreutils.GetRewriteRules(newTable, table.Info, 0, true)
				rewriteRules.Data = append(rewriteRules.Data, rules.Data...)
				tableIDMap[table.Info.ID] = int64(tableID)
			}
			// Validate rewrite rules
			for _, file := range files {
				err = restoreutils.ValidateFileRewriteRule(file, rewriteRules)
				if err != nil {
					return errors.Trace(err)
				}
			}
			cmd.Println("Check backupmeta done")
			return nil
		},
	}
	command.Flags().Uint64("offset", 0, "the offset of table id alloctor")
	return command
}

func decodeBackupMetaCommand() *cobra.Command {
	decodeBackupMetaCmd := &cobra.Command{
		Use:   "decode",
		Short: "decode backupmeta to json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			var cfg task.Config
			if err := cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}
			_, s, backupMeta, err := task.ReadBackupMeta(ctx, metautil.MetaFile, &cfg)
			if err != nil {
				return errors.Trace(err)
			}

			fieldName, _ := cmd.Flags().GetString("field")
			if fieldName == "" {
				if err := metautil.DecodeMetaFile(ctx, s, &cfg.CipherInfo, backupMeta.FileIndex); err != nil {
					return errors.Trace(err)
				}
				if err := metautil.DecodeMetaFile(ctx, s, &cfg.CipherInfo, backupMeta.RawRangeIndex); err != nil {
					return errors.Trace(err)
				}
				if err := metautil.DecodeMetaFile(ctx, s, &cfg.CipherInfo, backupMeta.SchemaIndex); err != nil {
					return errors.Trace(err)
				}
				if err := metautil.DecodeStatsFile(ctx, s, &cfg.CipherInfo, backupMeta.Schemas); err != nil {
					return errors.Trace(err)
				}

				// No field flag, write backupmeta to external storage in JSON format.
				backupMetaJSON, err := utils.MarshalBackupMeta(backupMeta)
				if err != nil {
					return errors.Trace(err)
				}
				err = s.WriteFile(ctx, metautil.MetaJSONFile, backupMetaJSON)
				if err != nil {
					return errors.Trace(err)
				}
				cmd.Printf("backupmeta decoded at %s\n", path.Join(s.URI(), metautil.MetaJSONFile))
				return nil
			}

			switch fieldName {
			// To be compatible with older BR.
			case "start-version":
				fieldName = "StartVersion"
			case "end-version":
				fieldName = "EndVersion"
			}

			_, found := reflect.TypeOf(*backupMeta).FieldByName(fieldName)
			if !found {
				cmd.Printf("field '%s' not found\n", fieldName)
				return nil
			}
			field := reflect.ValueOf(*backupMeta).FieldByName(fieldName)
			if !field.CanInterface() {
				cmd.Printf("field '%s' can not print\n", fieldName)
			} else {
				cmd.Printf("%v\n", field.Interface())
			}
			return nil
		},
	}

	decodeBackupMetaCmd.Flags().String("field", "", "decode specified field")

	return decodeBackupMetaCmd
}

func encodeBackupMetaCommand() *cobra.Command {
	encodeBackupMetaCmd := &cobra.Command{
		Use:   "encode",
		Short: "encode backupmeta json file to backupmeta",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			var cfg task.Config
			if err := cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}
			_, s, err := task.GetStorage(ctx, cfg.Storage, &cfg)
			if err != nil {
				return errors.Trace(err)
			}

			metaData, err := s.ReadFile(ctx, metautil.MetaJSONFile)
			if err != nil {
				return errors.Trace(err)
			}

			backupMetaJSON, err := utils.UnmarshalBackupMeta(metaData)
			if err != nil {
				return errors.Trace(err)
			}
			if backupMetaJSON.Version == metautil.MetaV2 {
				return errors.Errorf("encoding backupmeta v2 is unimplemented")
			}
			backupMeta, err := proto.Marshal(backupMetaJSON)
			if err != nil {
				return errors.Trace(err)
			}

			fileName := metautil.MetaFile
			if ok, _ := s.FileExists(ctx, fileName); ok {
				// Do not overwrite origin meta file
				fileName += "_from_json"
			}

			encryptedContent, iv, err := metautil.Encrypt(backupMeta, &cfg.CipherInfo)
			if err != nil {
				return errors.Trace(err)
			}

			err = s.WriteFile(ctx, fileName, append(iv, encryptedContent...))
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		},
	}
	return encodeBackupMetaCmd
}

func setPDConfigCommand() *cobra.Command {
	pdConfigCmd := &cobra.Command{
		Use:   "reset-pd-config-as-default",
		Short: "reset pd config adjusted by BR to default value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			var cfg task.Config
			if err := cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}

			mgr, err := task.NewMgr(ctx, tidbGlue, cfg.PD, cfg.TLS, task.GetKeepalive(&cfg),
				cfg.CheckRequirements, false, conn.NormalVersionChecker)
			if err != nil {
				return errors.Trace(err)
			}
			defer mgr.Close()

			if err := mgr.UpdatePDScheduleConfig(ctx); err != nil {
				return errors.Annotate(err, "fail to update PD merge config")
			}
			log.Info("add pd configs succeed")
			return nil
		},
	}
	return pdConfigCmd
}

func searchStreamBackupCommand() *cobra.Command {
	searchBackupCMD := &cobra.Command{
		Use:   "search-log-backup",
		Short: "search log backup by key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			searchKey, err := cmd.Flags().GetString("search-key")
			if err != nil {
				return errors.Trace(err)
			}
			if searchKey == "" {
				return errors.New("key param can't be empty")
			}
			keyBytes, err := hex.DecodeString(searchKey)
			if err != nil {
				return errors.Trace(err)
			}

			startTs, err := cmd.Flags().GetUint64("start-ts")
			if err != nil {
				return errors.Trace(err)
			}
			endTs, err := cmd.Flags().GetUint64("end-ts")
			if err != nil {
				return errors.Trace(err)
			}

			var cfg task.Config
			if err = cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}
			_, s, err := task.GetStorage(ctx, cfg.Storage, &cfg)
			if err != nil {
				return errors.Trace(err)
			}
			comparator := stream.NewStartWithComparator()
			bs := stream.NewStreamBackupSearch(s, comparator, keyBytes)
			bs.SetStartTS(startTs)
			bs.SetEndTs(endTs)

			kvs, err := bs.Search(ctx)
			if err != nil {
				return errors.Trace(err)
			}

			kvsBytes, err := json.MarshalIndent(kvs, "", "  ")
			if err != nil {
				return errors.Trace(err)
			}

			cmd.Println("search result")
			cmd.Println(string(kvsBytes))

			return nil
		},
	}

	flags := searchBackupCMD.Flags()
	flags.String("search-key", "", "hex encoded key")
	flags.Uint64("start-ts", 0, "search from start TSO, default is no start TSO limit")
	flags.Uint64("end-ts", 0, "search to end TSO, default is no end TSO limit")

	return searchBackupCMD
}
