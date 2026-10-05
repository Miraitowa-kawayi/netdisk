// Package config 聚合运行期配置：优先环境变量，其次读 .env 文件。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config 是服务启动所需的全部配置。
type Config struct {
	Addr          string
	DatabaseURL   string
	JWTSecret     string
	JWTTTL        time.Duration
	StorageDriver string
	StorageDir    string
}

const dotEnvFile = ".env"

// Load 先加载 .env（不存在则跳过）再读环境变量；已存在的环境变量优先，不被 .env 覆盖。
func Load() (*Config, error) {
	if err := loadDotEnv(dotEnvFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load %s: %w", dotEnvFile, err)
	}

	ttl, err := time.ParseDuration(envOr("NETDISK_JWT_TTL", "24h"))
	if err != nil {
		return nil, fmt.Errorf("parse NETDISK_JWT_TTL: %w", err)
	}

	cfg := &Config{
		Addr:          envOr("NETDISK_ADDR", ":8080"),
		DatabaseURL:   envOr("NETDISK_DATABASE_URL", ""),
		JWTSecret:     envOr("NETDISK_JWT_SECRET", ""),
		JWTTTL:        ttl,
		StorageDriver: envOr("NETDISK_STORAGE_DRIVER", "local"),
		StorageDir:    envOr("NETDISK_STORAGE_DIR", "./data/blobs"),
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "NETDISK_DATABASE_URL")
	}
	if cfg.JWTSecret == "" {
		missing = append(missing, "NETDISK_JWT_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少必填配置 %s（先 cp .env.example .env）", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// loadDotEnv 是最小的 KEY=VALUE 解析器：忽略空行与 # 注释，去掉值两端引号，
// 且不覆盖已存在的环境变量。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return sc.Err()
}
