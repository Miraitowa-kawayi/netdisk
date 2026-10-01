.PHONY: run build test fmt vet up up-all down reset logs psql seed

# 起服务（读 .env）
run:
	go run ./cmd/server

build:
	go build -o bin/netdisk ./cmd/server

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

# 只起数据库
up:
	docker compose up -d postgres

# 数据库 + 对象存储（P6 用）
up-all:
	docker compose --profile objectstore up -d

down:
	docker compose down

# 重建数据库：改过 migrations/*.sql 之后用这个（脚本只在空数据卷时执行）
reset:
	docker compose down -v
	docker compose up -d postgres

logs:
	docker compose logs -f postgres

# 直接进 psql 看数据
psql:
	docker exec -it netdisk-postgres psql -U netdisk -d netdisk
