#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

SCRIPT_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
BASE=/opt/rykvo-voice
MANAGER=/opt/rykvo-manager
STATE=/var/lib/rykvo-voice
BACKUPS=/var/backups/rykvo-voice
UNIT=/etc/systemd/system/rykvo-auth.service
SITE=/etc/nginx/sites-available/rykvo-voice
ENABLED=/etc/nginx/sites-enabled/rykvo-voice
DB=rykvo_voice
DB_URL='postgres:///rykvo_voice?host=/var/run/postgresql&user=rykvo_voice'
TEMP=
BACKUP=
SWITCHING=0
WAS_ACTIVE=0
SOURCE=${RYKVO_BUNDLE:-$SCRIPT_ROOT}

say() { printf '\n%s\n' "$*"; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }
exists() { [[ -e "$1" || -L "$1" ]]; }
db_sql() { runuser -u postgres -- psql -XAtq -v ON_ERROR_STOP=1 -d postgres -c "$1"; }
app_sql() { runuser -u postgres -- psql -XAtq -v ON_ERROR_STOP=1 -d "$DB" -c "$1"; }

remove_app_path() {
    local path=$1 resolved
    [[ ! -L "$path" ]] || { unlink -- "$path"; return; }
    resolved=$(realpath -m -- "$path")
    case "$resolved" in
        "$BASE/releases/"*|"$BASE/live"|"$STATE"|/var/cache/rykvo-voice|"$MANAGER") ;;
        *) die "拒绝清理范围外路径：$resolved" ;;
    esac
    [[ "$resolved" != "$BASE/releases/" ]] || die '清理路径无效'
    rm -rf --one-file-system -- "$resolved"
}

cleanup() {
    local result=$?
    trap - EXIT
    if (( SWITCHING )); then
        say '发布未完成，恢复上一个程序和站点配置。'
        rollback || printf '请检查备份：%s\n' "$BACKUP" >&2
    fi
    if [[ -n "$TEMP" && "$TEMP" == /var/tmp/rykvo-* && ! -L "$TEMP" ]]; then
        rm -rf --one-file-system -- "$TEMP"
    fi
    exit "$result"
}

check_system() {
    [[ $EUID == 0 ]] || die '请使用 sudo bash install.sh 或 root 登录'
    [[ -d /run/systemd/system ]] || die '需要使用 systemd 的服务器'
    # shellcheck disable=SC1091
    source /etc/os-release
    case "$ID:$VERSION_ID" in
        ubuntu:24.04|ubuntu:26.04|debian:13) ;;
        *) die '支持 Ubuntu 24.04/26.04、Debian 13' ;;
    esac
    case "$(uname -m)" in
        x86_64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) die '支持 amd64、arm64' ;;
    esac
    command -v flock >/dev/null || die '缺少 util-linux'
    exec 9>/run/lock/rykvo-voice.lock
    flock -n 9 || die '已有部署操作正在执行'
    TEMP=$(mktemp -d /var/tmp/rykvo-XXXXXXXX)
    chmod 700 "$TEMP"
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
}

packages() {
    say '安装运行环境'
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y --no-install-recommends ca-certificates curl python3 nginx postgresql openssl tar gzip xz-utils
    systemctl enable --now postgresql
    local version
    version=$(db_sql 'SHOW server_version_num')
    (( version >= 160000 )) || die '需要 PostgreSQL 16 或更新版本'
}

preflight() {
    if [[ -f "$UNIT" ]] && ! grep -q 'ExecStart=/opt/rykvo-voice/' "$UNIT"; then
        die '同名服务不属于本项目'
    fi
    if [[ -f "$SITE" ]] && ! grep -q '127.0.0.1:8080' "$SITE"; then
        die '同名 Nginx 配置不属于本项目'
    fi
    local file
    for file in /etc/nginx/sites-enabled/*; do
        exists "$file" || continue
        [[ "$file" == "$ENABLED" || "$file" == /etc/nginx/sites-enabled/default ]] || die '80 端口已有其他站点，请使用独立服务器'
    done
    if systemctl is-active --quiet rykvo-auth; then
        WAS_ACTIVE=1
    elif ss -H -ltn 'sport = :8080' | grep -q .; then
        die '8080 端口已被其他程序占用'
    fi
    if ss -H -ltn 'sport = :80' | grep -q .; then
        systemctl is-active --quiet nginx || die '80 端口已被其他程序占用'
    fi
    [[ ! -e "$BASE/live" || -L "$BASE/live" ]] || die 'live 目录不是发布链接'
}

get_source() {
    [[ -f "$SOURCE/bin/rykvo-auth" && -d "$SOURCE/web" ]] && return
    local token=${RYKVO_GITHUB_TOKEN:-}
    if [[ -z "$token" ]] && command -v gh >/dev/null; then
        token=$(gh auth token 2>/dev/null || true)
        if [[ -z "$token" && -n "${SUDO_USER:-}" ]]; then
            token=$(runuser -u "$SUDO_USER" -- gh auth token 2>/dev/null || true)
        fi
    fi
    if [[ -z "$token" ]]; then
        read -rs -p 'GitHub 令牌（仅需本私有仓库 Contents: Read）：' token </dev/tty
        printf '\n' >/dev/tty
    fi
    SOURCE=$(printf '%s\n' "$token" | python3 "$SCRIPT_ROOT/deploy/release.py" fetch "$TEMP" "$ARCH")
    unset token RYKVO_GITHUB_TOKEN
    [[ -f "$SOURCE/install.sh" && -f "$SOURCE/bin/rykvo-auth" ]] || die '安装包缺少文件'
}

stage_release() {
    RELEASE=$(date -u +%Y%m%d-%H%M%S)-$(openssl rand -hex 3)
    CANDIDATE="$BASE/releases/$RELEASE"
    mkdir -p "$CANDIDATE"
    printf 'rykvo-release\n' > "$CANDIDATE/.managed"
    cp -a "$SOURCE/web" "$CANDIDATE/web"
    install -m 755 "$SOURCE/bin/rykvo-auth" "$CANDIDATE/rykvo-auth"
    install -m 755 "$SOURCE/bin/cloudflared" "$CANDIDATE/cloudflared"
    cp "$SOURCE/VERSION" "$SOURCE/manifest.json" "$CANDIDATE/"
    "$CANDIDATE/cloudflared" --version
    find "$CANDIDATE" -type d -exec chmod 755 {} +
}

snapshot() {
    BACKUP="$BACKUPS/$(date -u +%Y%m%d-%H%M%S)-$(openssl rand -hex 3)"
    install -d -m 700 "$BACKUPS" "$BACKUP"
    for pair in "unit:$UNIT" "nginx:$SITE"; do
        local name=${pair%%:*} path=${pair#*:}
        if exists "$path"; then cp -a "$path" "$BACKUP/$name"; fi
    done
    if [[ -L "$ENABLED" ]]; then readlink "$ENABLED" > "$BACKUP/enabled-link"; fi
    if [[ -L /etc/nginx/sites-enabled/default ]]; then readlink /etc/nginx/sites-enabled/default > "$BACKUP/default-link"; fi
    if [[ -L "$BASE/live" ]]; then readlink "$BASE/live" > "$BACKUP/live-link"; fi
    printf '%s\n' "$WAS_ACTIVE" > "$BACKUP/was-active"
    SWITCHING=1
    systemctl stop rykvo-auth 2>/dev/null || [[ ! -f "$UNIT" ]]
    if [[ "$(db_sql "SELECT 1 FROM pg_database WHERE datname='$DB'")" == 1 ]]; then
        runuser -u postgres -- pg_dump -Fc "$DB" > "$BACKUP/database.dump"
    fi
    if [[ -d "$STATE" ]]; then tar -C /var/lib -czf "$BACKUP/state.tar.gz" rykvo-voice; fi
    chmod -R go-rwx "$BACKUP"
    say "备份：$BACKUP"
}

secret() {
    local label=$1 value again
    [[ -r /dev/tty ]] || die '首次安装请使用 SSH 交互终端设置密码'
    read -rs -p "$label（8–128 字节，回车自动生成）：" value </dev/tty
    printf '\n' >/dev/tty
    if [[ -z "$value" ]]; then
        value=$(openssl rand -hex 16)
        printf '%s：%s\n请保存此密码。\n' "$label" "$value" >/dev/tty
    else
        read -rs -p '再输入一次：' again </dev/tty
        printf '\n' >/dev/tty
        [[ "$value" == "$again" ]] || die '两次密码不一致'
    fi
    local length
    length=$(printf %s "$value" | wc -c)
    (( length >= 8 && length <= 128 )) || die '密码长度需要为 8–128 字节'
    printf %s "$value"
}

database() {
    id "$DB" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$DB"
    [[ "$(db_sql "SELECT 1 FROM pg_roles WHERE rolname='$DB'")" == 1 ]] || runuser -u postgres -- createuser "$DB"
    [[ "$(db_sql "SELECT 1 FROM pg_database WHERE datname='$DB'")" == 1 ]] || runuser -u postgres -- createdb -O "$DB" "$DB"
    local unsafe
    unsafe=$(db_sql "SELECT rolsuper OR rolcreaterole OR rolcreatedb FROM pg_roles WHERE rolname='$DB'")
    [[ "$unsafe" == f ]] || die '应用数据库角色权限过大'
    runuser -u "$DB" -- env DATABASE_URL="$DB_URL" "$CANDIDATE/rykvo-auth" -migrate
    if [[ "$(app_sql 'SELECT count(*) FROM administrators')" == 0 ]]; then
        secret '管理员密码' | runuser -u "$DB" -- env DATABASE_URL="$DB_URL" "$CANDIDATE/rykvo-auth" -init
    fi
    if [[ "$(app_sql 'SELECT count(*) FROM visibility_security')" == 0 ]]; then
        secret '功能显示独立密码' | runuser -u "$DB" -- env DATABASE_URL="$DB_URL" "$CANDIDATE/rykvo-auth" -init-visibility
    fi
}

health() {
    local attempt
    for ((attempt = 0; attempt < 30; attempt++)); do
        if systemctl is-active --quiet rykvo-auth && curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1/ -o "$TEMP/health.html" && grep -q 'id="login-form"' "$TEMP/health.html"; then
            [[ "$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 3 -H 'Host: panel.example.com' http://127.0.0.1/)" == 404 ]] || return 1
            [[ "$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 3 -H 'Host: panel.example.com' http://127.0.0.1/gly)" == 200 ]] || return 1
            [[ "$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1/app.js)" == 401 ]] || return 1
            return 0
        fi
        sleep 1
    done
    return 1
}

rollback() {
    systemctl stop rykvo-auth || true
    local name path
    for name in unit nginx; do
        path=$UNIT; [[ "$name" != nginx ]] || path=$SITE
        if exists "$BACKUP/$name"; then cp -a "$BACKUP/$name" "$path"; else rm -f -- "$path"; fi
    done
    rm -f -- "$ENABLED"
    if [[ -f "$BACKUP/enabled-link" ]]; then ln -s "$(cat "$BACKUP/enabled-link")" "$ENABLED"; fi
    if [[ -f "$BACKUP/default-link" && ! -e /etc/nginx/sites-enabled/default ]]; then
        ln -s "$(cat "$BACKUP/default-link")" /etc/nginx/sites-enabled/default
    fi
    if [[ -f "$BACKUP/live-link" ]]; then
        ln -sfn "$(cat "$BACKUP/live-link")" "$BASE/.live-rollback"
        mv -Tf "$BASE/.live-rollback" "$BASE/live"
    elif [[ -L "$BASE/live" ]]; then unlink "$BASE/live"; fi
    systemctl daemon-reload
    nginx -t && systemctl reload nginx
    if (( WAS_ACTIVE )); then systemctl start rykvo-auth; fi
    SWITCHING=0
}

install_manager() {
    install -d -m 755 "$MANAGER/deploy"
    install -m 755 "$SOURCE/install.sh" "$MANAGER/.install.sh"
    mv -Tf "$MANAGER/.install.sh" "$MANAGER/install.sh"
    install -m 644 "$SOURCE/deploy/release.py" "$MANAGER/deploy/release.py"
    printf '#!/usr/bin/env bash\nexec /opt/rykvo-manager/install.sh "$@"\n' > /usr/local/bin/rykvo
    chmod 755 /usr/local/bin/rykvo
}

deploy() {
    preflight
    packages
    get_source
    stage_release
    snapshot
    database
    ln -s "$CANDIDATE" "$BASE/.live-$RELEASE"
    mv -Tf "$BASE/.live-$RELEASE" "$BASE/live"
    install -m 644 "$SOURCE/deploy/rykvo-auth.service" "$UNIT"
    install -m 644 "$SOURCE/deploy/nginx.conf" "$SITE"
    if [[ -L /etc/nginx/sites-enabled/default ]]; then
        readlink /etc/nginx/sites-enabled/default > "$BASE/default-site-link"
        unlink /etc/nginx/sites-enabled/default
    fi
    ln -sfn "$SITE" "$ENABLED"
    nginx -t
    systemctl daemon-reload
    systemctl enable --now nginx rykvo-auth
    systemctl reload nginx
    health || die '健康检查未通过'
    install_manager
    printf '%s\n' "$BACKUP" > "$BASE/latest-backup"
    SWITCHING=0
    say "完成：$RELEASE"
    say '局域网：http://服务器IP/  ·  域名：https://域名/gly'
    say '管理命令：sudo rykvo'
}

uninstall() {
    local purge=${1:-} confirm binding=''
    [[ -f "$UNIT" || -L "$BASE/live" ]] || die '尚未安装'
    preflight
    if [[ "$purge" == --purge ]]; then
        binding=$(app_sql "SELECT COALESCE(binding->>'domain','') FROM tunnel_settings")
        [[ -z "$binding" ]] || die '请先在云服务器页面注销云连接，再彻底卸载，避免遗留远端 DNS 和隧道'
        read -r -p '永久删除账号、数据库、配置。输入 DELETE rykvo_voice：' confirm </dev/tty
        [[ "$confirm" == 'DELETE rykvo_voice' ]] || die '已取消'
    else
        read -r -p '卸载程序，保留数据库和配置？[y/N] ' confirm </dev/tty
        [[ "$confirm" == y || "$confirm" == Y ]] || die '已取消'
    fi
    snapshot
    systemctl disable --now rykvo-auth
    rm -f -- "$UNIT" "$ENABLED" "$SITE"
    if [[ -f "$BASE/default-site-link" && ! -e /etc/nginx/sites-enabled/default ]]; then
        ln -s "$(cat "$BASE/default-site-link")" /etc/nginx/sites-enabled/default
    fi
    systemctl daemon-reload
    nginx -t
    systemctl reload nginx
    SWITCHING=0
    if [[ -L "$BASE/live" ]]; then unlink "$BASE/live"; fi
    local release
    for release in "$BASE"/releases/*; do
        [[ -f "$release/.managed" && ! -L "$release" ]] || continue
        remove_app_path "$release"
    done
    if [[ "$purge" == --purge ]]; then
        runuser -u postgres -- dropdb --if-exists --force "$DB"
        runuser -u postgres -- dropuser --if-exists "$DB"
        remove_app_path "$STATE"
        remove_app_path /var/cache/rykvo-voice
        say "数据已删除；恢复备份仍保留在 $BACKUP（包含敏感数据，目录仅 root 可读）。"
    else
        say '程序已卸载，账号、数据库、云配置与备份保留。云端隧道和 DNS 不会被删除。'
    fi
    say '系统软件包与其他服务未删除；再次安装运行 sudo rykvo install。'
}

status() {
    systemctl --no-pager --full status rykvo-auth nginx postgresql || true
    if [[ -f "$BASE/live/VERSION" ]]; then printf '\n版本：'; cat "$BASE/live/VERSION"; fi
    if [[ -f "$BASE/live/REVISION" ]]; then printf '提交：'; cat "$BASE/live/REVISION"; fi
    if [[ -f "$BASE/latest-backup" ]]; then printf '备份：'; cat "$BASE/latest-backup"; fi
}

main() {
    local action=${1:-} option=${2:-}
    case "$action" in
        -h|--help) printf '用法：sudo bash install.sh [install|update|uninstall [--purge]|status]\n'; return ;;
        '')
            printf 'Rykvo Voice\n1. 安装\n2. 更新\n3. 卸载（保留数据）\n4. 状态\n0. 退出\n'
            read -r -p '选择：' action </dev/tty
            case "$action" in 1) action=install;; 2) action=update;; 3) action=uninstall;; 4) action=status;; 0) return;; *) die '选择无效';; esac ;;
    esac
    [[ "$action" =~ ^(install|update|uninstall|status)$ ]] || die '操作无效'
    [[ -z "$option" || ( "$action" == uninstall && "$option" == --purge ) ]] || die '参数无效'
    check_system
    case "$action" in
        install) deploy ;;
        update) [[ -f "$UNIT" ]] || die '尚未安装，请先安装'; deploy ;;
        uninstall) uninstall "$option" ;;
        status) status ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
