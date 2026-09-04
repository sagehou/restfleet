module github.com/sagehou/restfleet/tools

go 1.27.0

toolchain go1.27.1

tool (
	github.com/bufbuild/buf/cmd/buf
	github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
	github.com/pressly/goose/v3/cmd/goose
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
	honnef.co/go/tools/cmd/staticcheck
)

require (
	buf.build/gen/go/bufbuild/bufplugin/protocolbuffers/go v1.36.11-20260626152828-968bf0468096.1 // indirect
	buf.build/gen/go/bufbuild/protodescriptor/protocolbuffers/go v1.36.11-20250109164928-1da0de137947.1 // indirect
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260709200747-435963d16310.1 // indirect
	buf.build/gen/go/bufbuild/registry/connectrpc/go v1.20.0-20260713175918-10d915f5b43b.1 // indirect
	buf.build/gen/go/bufbuild/registry/protocolbuffers/go v1.36.11-20260713175918-10d915f5b43b.1 // indirect
	buf.build/gen/go/pluginrpc/pluginrpc/protocolbuffers/go v1.36.11-20241007202033-cf42259fcbfc.1 // indirect
	buf.build/go/app v0.2.1-0.20260626143626-be153867abea // indirect
	buf.build/go/bufplugin v0.10.0 // indirect
	buf.build/go/bufprivateusage v0.1.0 // indirect
	buf.build/go/interrupt v1.1.0 // indirect
	buf.build/go/protovalidate v1.2.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	buf.build/go/spdx v0.2.0 // indirect
	buf.build/go/standard v0.1.1-0.20260325175353-2b287e071df5 // indirect
	cel.dev/expr v0.25.2 // indirect
	connectrpc.com/connect v1.20.0 // indirect
	connectrpc.com/otelconnect v0.9.0 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.23.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.14.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.9.0 // indirect
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/ClickHouse/ch-go v0.74.0 // indirect
	github.com/ClickHouse/clickhouse-go/v2 v2.48.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/andybalholm/brotli v1.2.3 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/bufbuild/buf v1.72.0 // indirect
	github.com/bufbuild/protocompile v0.14.2-0.20260716165721-bb5762d29672 // indirect
	github.com/bufbuild/protoplugin v0.0.0-20260414125817-25d1d281b46b // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cli/browser v1.3.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/cli v29.6.1+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.8 // indirect
	github.com/docker/go-connections v0.8.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dprotaso/go-yit v0.0.0-20220510233725-9ba8df137936 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/elastic/go-sysinfo v1.15.5 // indirect
	github.com/elastic/go-windows v1.0.2 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/getkin/kin-openapi v0.142.0 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.8.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.23.1 // indirect
	github.com/go-openapi/swag/jsonname v0.26.0 // indirect
	github.com/go-sql-driver/mysql v1.10.0 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/cel-go v0.29.2 // indirect
	github.com/google/go-containerregistry v0.21.7 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jdx/go-netrc v1.0.0 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/pgzip v1.2.6 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mfridman/xflag v0.1.0 // indirect
	github.com/microsoft/go-mssqldb v1.11.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/moby/api v1.55.0 // indirect
	github.com/moby/moby/client v0.5.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/oapi-codegen/oapi-codegen/v2 v2.8.0 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/paulmach/orb v0.13.0 // indirect
	github.com/petermattis/goid v0.0.0-20260716134002-a9b348f0a2b9 // indirect
	github.com/pierrec/lz4/v4 v4.1.29 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pressly/goose/v3 v3.28.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.60.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rs/cors v1.11.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/speakeasy-api/jsonpath v0.6.3 // indirect
	github.com/speakeasy-api/openapi v1.24.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/tidwall/btree v1.8.1 // indirect
	github.com/tursodatabase/libsql-client-go v0.0.0-20260528064733-9d5d30a29a60 // indirect
	github.com/vertica/vertica-sql-go v1.3.8 // indirect
	github.com/vmware-labs/yaml-jsonpath v0.3.2 // indirect
	github.com/ydb-platform/ydb-go-genproto v0.0.0-20260810123728-f0c151ab31b9 // indirect
	github.com/ydb-platform/ydb-go-sdk/v3 v3.151.1 // indirect
	github.com/ziutek/mymysql v1.5.4 // indirect
	go.lsp.dev/jsonrpc2 v0.10.0 // indirect
	go.lsp.dev/pkg v0.0.0-20210717090340-384b27a52fb2 // indirect
	go.lsp.dev/protocol v0.12.0 // indirect
	go.lsp.dev/uri v0.3.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260715232425-e75dac1f907d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260831171406-18b4a7587f8a // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	howett.net/plist v1.0.1 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.57.0 // indirect
	mvdan.cc/xurls/v2 v2.6.0 // indirect
	pluginrpc.com/pluginrpc v0.5.0 // indirect
)
