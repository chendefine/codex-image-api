#!/bin/sh
# Aligns the image's "app" user with APP_UID/APP_GID, then drops root (unless
# those are 0) and execs the service as that user. Runs on every container start, so it is idempotent.
set -eu

# Started with a non-root --user/user: nothing to remap, run as-is.
if [ "$(id -u)" != 0 ]; then
  exec "$@"
fi

uid=${APP_UID:-1000}
gid=${APP_GID:-1000}
# Ids must be canonical decimal: ownership is compared as strings against stat
# output, and a leading zero would let "00" pass as non-root while the kernel
# reads it as 0. 0 itself is allowed (e.g. rootless Docker/Podman, where
# container root maps to the host user).
check_id() {
  case "$2" in
    '' | *[!0-9]* | 0?*) echo "entrypoint: $1 must be a decimal id without leading zeros (got '$2')" >&2; exit 1 ;;
  esac
  # 4294967295 is (uid_t)-1; the length check keeps -gt from overflowing.
  if [ "${#2}" -gt 10 ] || [ "$2" -gt 4294967294 ]; then
    echo "entrypoint: $1 is out of range (got $2)" >&2
    exit 1
  fi
}
check_id APP_UID "$uid"
check_id APP_GID "$gid"

# Edit the passwd/group entries directly instead of usermod/groupmod: usermod -u
# recursively chowns the home directory, which would reach into the bind-mounted
# CODEX_HOME on the host. Duplicate ids are fine, lookups go by name.
sed -i "s/^app:x:[0-9]*:[0-9]*:/app:x:$uid:$gid:/" /etc/passwd
sed -i "s/^app:x:[0-9]*:/app:x:$gid:/" /etc/group

# Bind mounts are host files (auth.json is shared with the host's codex):
# never chown them, only verify they belong to APP_UID.
prune=""
for p in /home/app/.codex /home/app/.codex/auth.json /data/workspace; do
  [ -e "$p" ] && mountpoint -q "$p" || continue
  if [ "$(stat -c %u "$p")" != "$uid" ]; then
    echo "entrypoint: $p is owned by uid $(stat -c %u "$p"), expected APP_UID=$uid;" \
      "chown it on the host or fix APP_UID/APP_GID" >&2
    exit 1
  fi
  prune="$prune -path $p -prune -o"
done

# Container-local state follows the remapped user. -xdev keeps find out of
# mounted directories; $prune skips the mount points themselves.
# shellcheck disable=SC2086
find /home/app /data -xdev $prune \( ! -user "$uid" -o ! -group "$gid" \) -exec chown "$uid:$gid" {} +

exec setpriv --reuid=app --regid=app --init-groups "$@"
