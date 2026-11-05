#!/usr/bin/env bash
# 真实双模型评测：第一次运行会让你配置 key，之后每次都复用同一份配置。
#
# 配置存在哪里：
#   ~/.dustkeeper-review/            (0700)
#   ├── credentials.json             (0600)  两个 key
#   ├── config.json                  (0600)  Dustkeeper 配置（含 review 段）
#   ├── bin/review-eval                      编译产物（源码没变就复用）
#   └── results/real-<时间>.json              每次评测的原始输出
#
# 它和你正在跑的正式实例是两个地方：这里不碰
# ~/Library/Application Support/Dustkeeper-Managed 里的 config/state，
# 也不打开数据库、不动文件、不解除暂停。
#
# 用法：
#   ./scripts/review-eval-with-keys.sh                第一次：引导配置 → 计划 → 确认 → 调用
#   ./scripts/review-eval-with-keys.sh                之后：直接复用已保存的配置
#   ./scripts/review-eval-with-keys.sh --show         只看当前配置（不调用模型）
#   ./scripts/review-eval-with-keys.sh --configure    重新配置（旧的会备份）
#   ./scripts/review-eval-with-keys.sh --dry-run      只打印评测计划，0 次调用
#   ./scripts/review-eval-with-keys.sh --yes          不询问确认，直接调用
#
# 换位置：DK_REVIEW_HOME=/别的/路径 ./scripts/review-eval-with-keys.sh
#
# 校准放行闸门（默认 .9/.9，JEV 实测最高 0.72/0.81，所以默认下 allow 从不出现）：
#   MIN_CONF_PCT=70 MIN_ALLOW_PCT=80 ./scripts/review-eval-with-keys.sh
#
# 费用：默认 5 个用例，最多 5 次 JEV + 5 次 MiniMax 请求。

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 程序用 O_NOFOLLOW 逐级打开凭据文件，路径里只要有一段是符号链接就会报
# "not a directory"——而这个错误完全看不出是路径问题。macOS 上 /tmp 和 /var
# 本身就是符号链接，所以这里先解析成真实路径，并明确告诉用户改了什么。
RAW_CONF_DIR="${DK_REVIEW_HOME:-$HOME/.dustkeeper-review}"
CONF_DIR="$(python3 -c 'import os,sys;print(os.path.realpath(sys.argv[1]))' "$RAW_CONF_DIR")"

CRED="$CONF_DIR/credentials.json"
CONFIG="$CONF_DIR/config.json"
BIN="$CONF_DIR/bin/review-eval"
RESULTS="$CONF_DIR/results"

JEV_URL_DEFAULT="https://api.typesafe.ai/v1/systemone"
JEV_MODEL_DEFAULT="jev-1.14.0"
MINIMAX_URL_DEFAULT="https://api.minimaxi.com/anthropic/v1/messages"
MINIMAX_MODEL_DEFAULT="MiniMax-M3.1-Flash"
MAX_CASES_DEFAULT=5

MODE="run"
ASSUME_YES=0
RECONFIGURE=0
SHOW_ONLY=0

# 可选：校准 JEV 的放行闸门（默认 .9/.9）。
# 闸门是"两个条件同时满足"，而 JEV 在合成样本上的置信度最高只到 0.72，
# 所以默认值下 allow 从未出现——既不会删也不会移。想验证"双方同意即删除"
# 这条路是否真的走得通，用这两个环境变量把闸门调到测量得到的范围：
#   MIN_CONF_PCT=70 MIN_ALLOW_PCT=80 ./scripts/review-eval-with-keys.sh
# 不带这两个变量时，配置里不会写这两项，行为与之前完全一致。
MIN_CONF_PCT="${MIN_CONF_PCT:-}"
MIN_ALLOW_PCT="${MIN_ALLOW_PCT:-}"
# 评测里的单次请求超时。生产默认 30s；这里给 60s，是为了不把"模型答得慢"
# 记成"模型答错"——一次超时会让该用例从测量里消失。
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-60}"

# 记下调用者环境里原本有没有这两个 key。交互时要区分"来自环境变量"和
# "来自已保存的配置"——说错来源会让人以为自己配过，或者以为没配。
ENV_JEV="${TYPESAFE_API_KEY:-}"
ENV_MM="${MINIMAX_API_KEY:-}"

die() { printf '\033[31m停止：\033[0m%s\n' "$*" >&2; exit 2; }
say() { printf '%s\n' "$*"; }
head1() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

for arg in "$@"; do
  case "$arg" in
    --dry-run)   MODE="dry" ;;
    --yes|-y)    ASSUME_YES=1 ;;
    --configure) RECONFIGURE=1 ;;
    --show)      SHOW_ONLY=1 ;;
    -h|--help)   sed -n '2,31p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "不认识的参数：${arg}（可用：--show / --configure / --dry-run / --yes / --help）" ;;
  esac
done

[[ -f "$REPO/go.mod" ]] || die "看起来不是仓库根目录：${REPO}"
command -v python3 >/dev/null || die "找不到 python3"

if [[ "$CONF_DIR" != "$RAW_CONF_DIR" ]]; then
  say "配置目录已解析为真实路径：${RAW_CONF_DIR} → ${CONF_DIR}"
  say "（程序的凭据读取不支持路径中的符号链接）"
fi

file_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}

# ---------------------------------------------------------------------------
# 一个配置文件是否"看起来可用"（真正的校验交给程序本身）
# ---------------------------------------------------------------------------
config_usable() {
  [[ -f "$CONFIG" && -f "$CRED" ]] || return 1
  python3 - "$CONFIG" "$CRED" <<'PY' >/dev/null 2>&1
import json, sys
cfg = json.load(open(sys.argv[1], encoding="utf-8"))
rev = cfg.get("review") or {}
for key in ("credentials_file", "jev_url", "jev_model", "minimax_url", "minimax_model"):
    if not rev.get(key):
        raise SystemExit(1)
# 配置必须指向这一份凭据；否则"看着能用"其实是读别处。
if rev["credentials_file"] != sys.argv[2]:
    raise SystemExit(1)
cred = json.load(open(sys.argv[2], encoding="utf-8"))
for key in ("typesafe_api_key", "minimax_api_key"):
    if len(str(cred.get(key, ""))) < 16:
        raise SystemExit(1)
PY
}

print_config() {
  python3 - "$CONFIG" "$CRED" "$CONF_DIR" <<'PY'
import json, os, stat, sys
cfg = json.load(open(sys.argv[1], encoding="utf-8"))["review"]
cred = json.load(open(sys.argv[2], encoding="utf-8"))
home = sys.argv[3]

def tail(v):
    v = str(v)
    return f"长度 {len(v)}，末四位 …{v[-4:]}"

print(f"  位置     {home}")
print(f"  JEV      {cfg['jev_model']} @ {cfg['jev_url']}")
print(f"  代理     {cfg.get('jev_proxy_url') or '（未设置）'}")
print(f"  MiniMax  {cfg['minimax_model']} @ {cfg['minimax_url']}")
print(f"  用例数   {cfg.get('max_candidates', 5)}")
print(f"  单次超时 {cfg.get('timeout_seconds', 30)}s" + ("" if cfg.get('timeout_seconds') else "（生产默认）"))
print(f"  放行闸门 置信度 ≥ {cfg.get('jev_min_confidence_pct', 90)/100}，概率 ≥ {cfg.get('jev_min_allow_probability_pct', 90)/100}"
      + ("" if "jev_min_confidence_pct" in cfg or "jev_min_allow_probability_pct" in cfg else "（默认；JEV 实测最高 0.72/0.81，故 allow 不会出现）"))
print(f"  JEV key  {tail(cred['typesafe_api_key'])}")
print(f"  LLM key  {tail(cred['minimax_api_key'])}")

mode = stat.S_IMODE(os.stat(sys.argv[2]).st_mode)
print(f"  凭据权限 {mode:04o}" + ("" if mode == 0o600 else "  ← 不是 0600，程序会拒绝"))
PY
}

# ---------------------------------------------------------------------------
# 首次配置（交互；输入不回显）
# ---------------------------------------------------------------------------
probe_proxy() {
  local port
  for port in 7890 7897 1087 6152 8888 8080; do
    if nc -z -G 1 127.0.0.1 "$port" >/dev/null 2>&1; then
      printf 'http://127.0.0.1:%s' "$port"
      return
    fi
  done
}

ask_secret() {
  local var="$1" label="$2" origin="$3" current value
  current="${!var:-}"

  if [[ ! -t 0 ]]; then
    # 没有终端：只能用已有的值，且必须说清楚，不能静默顶替。
    [[ -n "$current" ]] || die "没有终端可以输入 ${label}；可以改用环境变量 ${var}=... 传一次"
    say "${label}：使用现有的值（${origin}，长度 ${#current}，末四位 …${current: -4}）"
    return
  fi

  if [[ -n "$current" ]]; then
    # 已有值也必须说出来：否则用户会以为"我没填，它就自己有了"。
    printf '%s：%s 已有值（长度 %d，末四位 …%s）\n' \
      "$label" "$origin" "${#current}" "${current: -4}" >&2
    printf '  直接回车沿用它，或粘贴新的（不回显）：' >&2
  else
    printf '%s（输入不回显）：' "$label" >&2
  fi

  while :; do
    IFS= read -rs value || value=""
    printf '\n' >&2
    if [[ -z "$value" ]]; then
      if [[ -n "$current" ]]; then
        say "  沿用这个值" >&2
        return
      fi
      printf '  不能为空，再来一次：' >&2
      continue
    fi
    if [[ ${#value} -lt 16 ]]; then
      printf '  只有 %d 个字符，看起来不是完整 key，再来一次：' "${#value}" >&2
      continue
    fi
    printf -v "$var" '%s' "$value"
    return
  done
}

ask_text() {
  # ask_text 变量名 "提示" 默认值
  local var="$1" label="$2" fallback="$3" value
  if [[ -n "${!var:-}" ]]; then return; fi
  [[ -t 0 ]] || { printf -v "$var" '%s' "$fallback"; return; }
  printf '%s [%s]：' "$label" "$fallback" >&2
  IFS= read -r value || value=""
  [[ -n "$value" ]] || value="$fallback"
  printf -v "$var" '%s' "$value"
}

interactive_configure() {
  head1 "配置双模型凭据"

  # 已经有配置时，把它当作"当前值"：--configure 是改，不是推倒重来。
  # 这样只换一个 key 时，其余各项回车即可保留。
  if [[ -f "$CONFIG" && -f "$CRED" ]]; then
    eval "$(python3 - "$CONFIG" "$CRED" <<'PY'
import json, shlex, sys
cfg = (json.load(open(sys.argv[1], encoding="utf-8")).get("review") or {})
cred = json.load(open(sys.argv[2], encoding="utf-8"))
pairs = {
    "TYPESAFE_API_KEY_PRESET": cred.get("typesafe_api_key", ""),
    "MINIMAX_API_KEY_PRESET": cred.get("minimax_api_key", ""),
    "JEV_URL_PRESET": cfg.get("jev_url", ""),
    "JEV_MODEL_PRESET": cfg.get("jev_model", ""),
    "JEV_PROXY_PRESET": cfg.get("jev_proxy_url", ""),
    "MINIMAX_URL_PRESET": cfg.get("minimax_url", ""),
    "MINIMAX_MODEL_PRESET": cfg.get("minimax_model", ""),
    "MAX_CASES_PRESET": str(cfg.get("max_candidates", 5)),
}
for key, value in pairs.items():
    print(f"{key}={shlex.quote(str(value))}")
PY
)"
    say "检测到已保存的配置：下面每一项回车即沿用当前值。"
  else
    say "这些值会写进 ${CONF_DIR}，之后每次运行都用它，不用再输。"
  fi
  say "直接回车表示采用方括号里的默认值。"
  say ""

  TYPESAFE_API_KEY="${TYPESAFE_API_KEY:-${TYPESAFE_API_KEY_PRESET:-}}"
  MINIMAX_API_KEY="${MINIMAX_API_KEY:-${MINIMAX_API_KEY_PRESET:-}}"
  local jev_origin="已保存的配置" mm_origin="已保存的配置"
  [[ -n "$ENV_JEV" ]] && jev_origin="环境变量 TYPESAFE_API_KEY"
  [[ -n "$ENV_MM" ]] && mm_origin="环境变量 MINIMAX_API_KEY"
  ask_secret TYPESAFE_API_KEY "JEV / TypeSafe key" "$jev_origin"
  ask_secret MINIMAX_API_KEY "MiniMax key" "$mm_origin"

  local detected
  detected="$(probe_proxy || true)"
  ask_text JEV_PROXY "JEV 代理（外部 JEV 必须走代理）" "${JEV_PROXY_PRESET:-${detected:-http://127.0.0.1:7890}}"

  ask_text JEV_URL "JEV 地址" "${JEV_URL_PRESET:-$JEV_URL_DEFAULT}"
  ask_text JEV_MODEL "JEV 模型名" "${JEV_MODEL_PRESET:-$JEV_MODEL_DEFAULT}"
  ask_text MINIMAX_URL "MiniMax 地址" "${MINIMAX_URL_PRESET:-$MINIMAX_URL_DEFAULT}"
  ask_text MINIMAX_MODEL "MiniMax 模型名" "${MINIMAX_MODEL_PRESET:-$MINIMAX_MODEL_DEFAULT}"
  ask_text MAX_CASES "这次跑几个用例（1..6）" "${MAX_CASES_PRESET:-$MAX_CASES_DEFAULT}"

  case "$MAX_CASES" in
    ''|*[!0-9]*) die "用例数必须是 1..6 的整数，拿到的是 ${MAX_CASES}" ;;
  esac
  [[ "$MAX_CASES" -ge 1 && "$MAX_CASES" -le 6 ]] || die "用例数必须在 1..6 之间，拿到的是 ${MAX_CASES}"

  mkdir -p "$CONF_DIR" "$CONF_DIR/bin" "$RESULTS"
  chmod 700 "$CONF_DIR" "$CONF_DIR/bin" "$RESULTS"

  # 重新配置时，旧文件改名保留，不悄悄覆盖。
  if [[ -e "$CRED" || -e "$CONFIG" ]]; then
    local stamp
    stamp="$(date +%Y%m%dT%H%M%S)"
    [[ -e "$CRED" ]] && mv "$CRED" "${CRED}.bak.${stamp}"
    [[ -e "$CONFIG" ]] && mv "$CONFIG" "${CONFIG}.bak.${stamp}"
    say "旧配置已备份为 *.bak.${stamp}"
  fi

  DK_JEV_KEY="$TYPESAFE_API_KEY" DK_MM_KEY="$MINIMAX_API_KEY" python3 - "$CRED" <<'PY'
import json, os, stat, sys
path = sys.argv[1]
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as f:
    json.dump({"typesafe_api_key": os.environ["DK_JEV_KEY"],
               "minimax_api_key": os.environ["DK_MM_KEY"]}, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
st = os.stat(path)
assert stat.S_IMODE(st.st_mode) == 0o600 and st.st_nlink == 1
PY
  # 程序会拒绝带 ACL 的凭据文件；顺手清掉可能存在的扩展属性。
  command -v xattr >/dev/null 2>&1 && { xattr -c "$CRED" 2>/dev/null || true; }

  DP_JEV_PROXY="$JEV_PROXY" DP_JEV_URL="$JEV_URL" DP_JEV_MODEL="$JEV_MODEL" \
  DP_MM_URL="$MINIMAX_URL" DP_MM_MODEL="$MINIMAX_MODEL" DP_CRED="$CRED" \
  DP_CASES="$MAX_CASES" DP_MIN_CONF="$MIN_CONF_PCT" DP_MIN_ALLOW="$MIN_ALLOW_PCT" \
  DP_TIMEOUT="$TIMEOUT_SECONDS" \
  python3 - "$CONFIG" <<'PY'
import json, os, sys
# config.Load 用 StrictStruct：字段必须齐全且只能是认识的键，所以 runtime 段写全。
cfg = {
    "config_version": 1,
    "runtime": {
        "scan_batch_size": 500,
        "io_workers": 1,
        "io_buffer_bytes": 1048576,
        "read_rate_bytes_per_second": 20971520,
        "run_soft_seconds": 60,
        "run_read_budget_bytes": 1073741824,
        "max_auto_quarantine_files": 100,
        "max_auto_quarantine_bytes": 2147483648,
        "max_auto_purge_files": 100,
        "max_auto_purge_bytes": 1073741824,
        "min_free_bytes": 536870912,
        "min_free_percent": 5,
    },
    "model": {
        "enabled": False,
        "provider": "chat_completions_json",
        "base_url": "https://api.example.invalid/v1",
        "model": "explicitly-configured-model",
        "api_key_env": "DUSTKEEPER_MODEL_API_KEY",
        "request_timeout_seconds": 20,
        "max_response_bytes": 65536,
    },
    "review": {
        "credentials_file": os.environ["DP_CRED"],
        "jev_url": os.environ["DP_JEV_URL"],
        "jev_model": os.environ["DP_JEV_MODEL"],
        "minimax_url": os.environ["DP_MM_URL"],
        "minimax_model": os.environ["DP_MM_MODEL"],
        "max_candidates": int(os.environ["DP_CASES"]),
    },
}
proxy = os.environ.get("DP_JEV_PROXY", "")
if proxy:
    cfg["review"]["jev_proxy_url"] = proxy
# 只有显式给了才写：absent 与「写成默认值」在程序里等价，但 absent 更诚实——
# 配置里出现 .9 会让人以为有人校准过。
if os.environ.get("DP_MIN_CONF"):
    cfg["review"]["jev_min_confidence_pct"] = int(os.environ["DP_MIN_CONF"])
if os.environ.get("DP_MIN_ALLOW"):
    cfg["review"]["jev_min_allow_probability_pct"] = int(os.environ["DP_MIN_ALLOW"])
cfg["review"]["timeout_seconds"] = int(os.environ.get("DP_TIMEOUT", "60"))
with open(sys.argv[1], "w", encoding="utf-8") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  chmod 600 "$CONFIG"

  head1 "配置已保存"
  print_config
  say ""
  say "以后直接运行这个脚本就会复用上面这份配置。改配置用 --configure。"
}

# ---------------------------------------------------------------------------
# 1. 确认配置（没有就配置，有就复用）
# ---------------------------------------------------------------------------
if [[ "$SHOW_ONLY" == "1" ]]; then
  if ! config_usable; then
    die "还没有可用的配置：先运行一次 ${0##*/} 完成配置"
  fi
  head1 "当前配置"
  print_config
  exit 0
fi

if [[ "$RECONFIGURE" == "1" ]] || ! config_usable; then
  interactive_configure
else
  head1 "复用已保存的配置"
  # 权限被改宽是常见事故，程序会直接拒绝，这里先收紧并说明。
  mode="$(file_mode "$CRED")"
  if [[ "$mode" != "600" ]]; then
    chmod 600 "$CRED"
    say "凭据权限原来是 ${mode}，已收紧为 0600（程序只接受 0600）"
  fi
  print_config
fi

# ---------------------------------------------------------------------------
# 2. 准备评测程序（源码没动就复用上次编译的）
# ---------------------------------------------------------------------------
head1 "准备评测程序"
mkdir -p "$CONF_DIR/bin"
if [[ -n "${REVIEW_EVAL_BIN:-}" ]]; then
  [[ -x "$REVIEW_EVAL_BIN" ]] || die "REVIEW_EVAL_BIN 不可执行：${REVIEW_EVAL_BIN}"
  BIN="$REVIEW_EVAL_BIN"
  say "使用指定二进制：${BIN}"
# 注意：不能只比 *.go。评测样本是用 go:embed 编进二进制的，只改 JSON 时
# 旧二进制会带着旧标注继续跑，测出来的"命中"就是假的。
elif [[ -x "$BIN" ]] && [[ -z "$(find "$REPO/cmd/review-eval" "$REPO/internal" -newer "$BIN" -print -quit 2>/dev/null)" ]]; then
  say "复用已编译的二进制：${BIN}"
else
  command -v go >/dev/null || die "找不到 go（需要编译 cmd/review-eval）"
  (cd "$REPO" && go build -o "$BIN" ./cmd/review-eval) || die "编译失败"
  say "已编译：${BIN}"
fi

MAX_CASES="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["review"].get("max_candidates",5))' "$CONFIG")"

# ---------------------------------------------------------------------------
# 3. 计划（0 次调用）
# ---------------------------------------------------------------------------
head1 "评测计划（这一句之前没有任何模型调用）"
set +e
"$BIN" -config "$CONFIG" -max-cases "$MAX_CASES"
plan_status=$?
set -e
if [[ $plan_status -ne 0 ]]; then
  die "计划阶段就失败了（退出码 ${plan_status}）。上面是程序自己的诊断。"
fi

if [[ "$MODE" == "dry" ]]; then
  say ""
  say "计划如上，未调用任何模型。去掉 --dry-run 就会真实请求。"
  exit 0
fi

# ---------------------------------------------------------------------------
# 4. 真实调用
# ---------------------------------------------------------------------------
if [[ "$ASSUME_YES" -ne 1 ]]; then
  [[ -t 0 ]] || die "没有终端可以确认；确定要调用就加 --yes"
  printf '\n即将真实调用，最多 %s 次 JEV + %s 次 MiniMax。继续？[y/N] ' "$MAX_CASES" "$MAX_CASES" >&2
  IFS= read -r answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) say "已取消，未发生任何模型调用。"; exit 0 ;;
  esac
fi

mkdir -p "$RESULTS"
OUT="$RESULTS/real-$(date +%Y%m%dT%H%M%S).json"

say ""
say "调用中……原始输出同时写入 ${OUT}"
set +e
"$BIN" -config "$CONFIG" -execute -max-cases "$MAX_CASES" | tee "$OUT"
run_status=${PIPESTATUS[0]}
set -e

# ---------------------------------------------------------------------------
# 5. 结论：把退出码翻译成人话，并写清它不证明什么
# ---------------------------------------------------------------------------
head1 "结论"
python3 - "$OUT" <<'PY'
import json, sys
cases, summary = [], None
with open(sys.argv[1], encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if obj.get("type") == "summary":
            summary = obj
        elif obj.get("type") == "case":
            cases.append(obj)

if summary is None:
    print("没有拿到 summary：这次运行在总结之前就中止了。")
    raise SystemExit(1)

for c in cases:
    mark = "对" if c.get("match") else "错"
    print(f"  [{mark}] {c.get('id'):<28} 期望 {c.get('expected'):<12} 实际 {c.get('actual', c.get('error', '?'))}")
    if c.get("error_detail"):
        print(f"       错误：{c['error_detail']}")
    if c.get("reason"):
        print(f"       理由：{c['reason']}")

print()
counts = (f"用例 {summary.get('attempted', 0)}/{summary.get('selected', 0)}，"
          f"不匹配 {summary.get('mismatches', 0)}，弃权 {summary.get('abstentions', 0)}")
if summary.get("errors"):
    counts += f"，请求失败 {summary['errors']}"
print(counts)
if summary.get("stop_reason"):
    print(f"停止原因：{summary['stop_reason']}")
if summary.get("jev_models") or summary.get("llm_models"):
    print(f"实际使用的模型：{','.join(summary.get('jev_models') or [])} + {','.join(summary.get('llm_models') or [])}")
print()

reason = summary.get("stop_reason")
mismatches = summary.get("mismatches", 0)
abstentions = summary.get("abstentions", 0)
errors = summary.get("errors", 0)
attempted = summary.get("attempted", 0)

if reason == "model_unavailable":
    print("解读：一个用例都没跑通（凭据、代理、端点或格式）。")
    print("      这不说明模型判得对不对，只说明请求没成功。")
elif reason == "model_error":
    # Partial failure is not the same finding as a broken configuration: the
    # cases that did run are real measurements and must be read as such.
    print(f"解读：{errors} 个用例的请求失败，其余 {attempted - errors} 个跑通了，上面的判定只针对跑通的那几条。")
    print("      失败那几条的原因见每条的「错误：」行——超时、限流、格式错误要分开处理。")
elif reason == "false_allow":
    print("解读：模型对一个应当保留的用例说了 allow。这是要当缺陷处理的结果，")
    print("      不是可以接受的误差——产品的删除路径建立在'双方都同意'上。")
elif mismatches:
    print(f"解读：模型跑通了，有 {mismatches} 条与标注不符。")
    if abstentions:
        print(f"      另有 {abstentions} 条是弃权（模型说证据不足）。")
elif abstentions:
    # Zero mismatches with abstentions is a different finding from "did not
    # match": the labels were met, the reviewer simply declined to decide.
    print(f"解读：{attempted - errors} 条全部与标注一致，其中 {abstentions} 条是模型说证据不足。")
    print("      在产品里弃权是安全的一侧（不会删），评测仍把它算作未结论，所以退出码是 1 而不是 0。")
else:
    print("解读：全部用例命中。")
print()
print("这个结果能说明什么：上面的判定是两个模型的实际返回，不是脚本按规则推演的。")
print("这个结果不能说明什么：它只在人工构造的合成样本上成立，不是真实文件上的评测，")
print("也不等于'已获得真实双模型同意'，更不改变正式实例仍然 paused 的事实。")
PY

say ""
say "原始记录：${OUT}"
case "$run_status" in
  0) say "退出码 0：全部命中且无弃权。" ;;
  1) say "退出码 1：跑通了，但有未结论的用例（不匹配或弃权，见上面的计数）。" ;;
  *) say "退出码 ${run_status}：至少一次请求没有成功（见上面的「请求失败」与「错误：」行）。" ;;
esac
exit "$run_status"
