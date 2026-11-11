#!/usr/bin/env bash
# 本地把文件/目录加密成 OpenList crypt 格式（rclone crypt 标准），
# 加密产物可以直接用「中国移动云盘」客户端上传，不需要经过 OpenList 中转。
#
# 用法:
#   ./encrypt-to-139.sh <源文件或目录> [输出目录]
#
# 环境变量:
#   OPENLIST_DB          OpenList 的 data.db 路径
#   OPENLIST_BIN         openlist 可执行文件
#   OPENLIST_CRYPT_MOUNT crypt 存储的挂载路径（默认 /加密存储）
#
# 说明: 加密参数（password / salt / 文件名加密方式 / 目录名加密 / 后缀）
# 全部从数据库里对应存储的配置读取，保证与 OpenList 侧完全一致，
# 否则传上去的文件会因为名字解不开而被过滤掉。
set -euo pipefail

# Git Bash 会把 "/加密存储" 这类参数当成 Unix 路径改写成本机路径，
# 关掉它的自动转换，否则查不到存储。
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SRC="${1:-}"
DST="${2:-}"
if [ -z "$SRC" ]; then
  echo "用法: $0 <源文件或目录> [输出目录]" >&2
  exit 1
fi
if [ ! -e "$SRC" ]; then
  echo "源路径不存在: $SRC" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DB="${OPENLIST_DB:-$HOME/AppData/Roaming/OpenList Desktop/data/data.db}"
BIN="${OPENLIST_BIN:-$ROOT/openlist-debug.exe}"
MOUNT="${OPENLIST_CRYPT_MOUNT:-/加密存储}"

if [ ! -f "$DB" ]; then
  echo "找不到数据库: $DB（可用 OPENLIST_DB 指定）" >&2
  exit 1
fi
if [ ! -x "$BIN" ]; then
  echo "找不到可执行文件: $BIN（可用 OPENLIST_BIN 指定）" >&2
  exit 1
fi

read -r PWD_OBF SALT_OBF FN_ENC DIR_ENC FN_ENCODING SUFFIX REMOTE CLIENT_DIR <<< "$(python - "$DB" "$MOUNT" <<'PY'
import sqlite3, json, sys
db, mount = sys.argv[1], sys.argv[2]
con = sqlite3.connect(db)
row = con.execute(
    "select addition from x_storages where mount_path=? and driver='Crypt'", (mount,)
).fetchone()
if not row:
    sys.exit("数据库里找不到 crypt 存储: " + mount)
a = json.loads(row[0])
remote = a.get("remote_path", "")
# remote_path 形如 /10086移动/加密，去掉底层挂载点前缀后就是客户端里要上传的目录
mnts = [r[0] for r in con.execute(
    "select mount_path from x_storages where driver in ('139Yun','139YunShare')")]
client_dir = remote
for m in sorted(mnts, key=len, reverse=True):
    if remote.startswith(m):
        client_dir = remote[len(m):] or "/"
        break
print(
    a["password"], a["salt"], a["filename_encryption"],
    a["directory_name_encryption"], a["filename_encoding"],
    a["encrypted_suffix"], remote, client_dir,
)
PY
)"

if [ -z "${PWD_OBF:-}" ]; then
  echo "读取 crypt 存储参数失败，请检查 OPENLIST_DB 与 OPENLIST_CRYPT_MOUNT" >&2
  exit 1
fi

if [ -z "$DST" ]; then
  if [ -d "$SRC" ]; then
    DST="${SRC%/}_crypt"
  else
    DST="$(dirname "$SRC")"
  fi
fi

echo "== 加密参数 =="
echo "  存储      : $MOUNT"
echo "  文件名加密: $FN_ENC（$FN_ENCODING）"
echo "  目录名加密: $DIR_ENC"
echo "  后缀      : $SUFFIX"
echo "  源        : $SRC"
echo "  输出      : $DST"
echo

LOG="$(mktemp)"
set +e
"$BIN" crypt -s "$SRC" -d "$DST" --op=en \
  --pwd="$PWD_OBF" --salt="$SALT_OBF" \
  --filename-encrypt="$FN_ENC" --dirname-encrypt="$DIR_ENC" \
  --filename-encode="$FN_ENCODING" --suffix="$SUFFIX" 2>"$LOG"
RC=$?
set -e
# 旧版二进制会把 password/salt 打进 config 日志，这里过滤掉
grep -v "config:directory_name_encryption" "$LOG" >&2 || true
rm -f "$LOG"
if [ "$RC" -ne 0 ]; then
  echo "加密失败，退出码 $RC" >&2
  exit "$RC"
fi

echo
echo "== 完成 =="
echo "把上面输出目录里的内容原样上传到中国移动云盘的「${CLIENT_DIR#/}」文件夹"
echo "（对应 OpenList 里的 $MOUNT），然后在 OpenList 里刷新即可看到解密后的文件。"
echo "注意: 文件名和目录名都不要手动改，改了就解不开了。"
