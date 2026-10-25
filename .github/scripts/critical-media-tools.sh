#!/usr/bin/env bash
# Use the shipped worker's FFmpeg 7/SVT runtime without running its worker.
# Ubuntu 24's FFmpeg 6 cannot honor the AV1 keyframes required by our HLS.
set -euo pipefail

case "${1:-}" in
  start)
    : "${RUNNER_TEMP:?}" "${GITHUB_ENV:?}" "${GITHUB_PATH:?}"
    state=$(mktemp -d "$RUNNER_TEMP/contentkit-media-tools.XXXXXX")
    mkdir "$state/bin" "$state/tmp"
    printf 'CONTENTKIT_MEDIA_TOOLS_DIR=%s\nTMPDIR=%s/tmp\n' "$state" "$state" >> "$GITHUB_ENV"
    printf '%s/bin\n' "$state" >> "$GITHUB_PATH"
    docker --context default run -d --init --cidfile "$state/container.id" \
      --label "contentkit.critical-media-tools=$state" \
      --network host --read-only --cap-drop ALL --security-opt no-new-privileges \
      --cpus 2 --memory 1g --user "$(id -u):$(id -g)" \
      --mount "type=bind,source=$state/tmp,target=$state/tmp" \
      --entrypoint /bin/sleep \
      openrails/contentkit-media-worker@sha256:33e8a30d3c561d0e0117e97a68ef0af8cd4939dda6eb4831737ee3c31f05e7b6 \
      infinity
    cat > "$state/bin/media-tool" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
state=${CONTENTKIT_MEDIA_TOOLS_DIR:?}
read -r container < "$state/container.id"
tool=${0##*/}
case "$tool" in ffmpeg|ffprobe) ;; *) exit 64 ;; esac
pidfile=$(mktemp "$state/tmp/$tool-pid.XXXXXX")
trap 'rm -f -- "$pidfile" "$pidfile.cancel"' EXIT
forward() {
  # The marker closes cancellation before the exec has written its PID.
  : > "$pidfile.cancel"
  if read -r pid < "$pidfile" && [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then
    docker --context default exec "$container" /bin/sh -c 'kill -s "$1" "$2"' sh "$1" "$pid" >/dev/null 2>&1 || true
  fi
}
trap 'forward TERM' TERM
trap 'forward INT' INT
docker --context default exec "$container" /bin/sh -c '
  printf "%s\n" "$$" > "$1"
  test ! -e "$1.cancel" || exit 130
  shift
  exec "$@"
' sh "$pidfile" "/usr/bin/$tool" "$@" &
child=$!
set +e
while true; do
  wait "$child"
  status=$?
  # A trapped signal interrupts wait before Docker has delivered the result.
  kill -0 "$child" 2>/dev/null || break
done
exit "$status"
WRAPPER
    chmod +x "$state/bin/media-tool"
    ln -s media-tool "$state/bin/ffmpeg"
    ln -s media-tool "$state/bin/ffprobe"
    docker --context default exec "$(cat "$state/container.id")" /usr/bin/ffmpeg -version
    ;;
  stop)
    state=${CONTENTKIT_MEDIA_TOOLS_DIR:-}
    test -n "$state" && test -f "$state/container.id" || exit 0
    read -r container < "$state/container.id"
    [[ "$container" =~ ^[a-f0-9]{64}$ ]] || exit 1
    if ! owner=$(docker --context default inspect --format '{{index .Config.Labels "contentkit.critical-media-tools"}}' "$container" 2>/dev/null); then
      # Distinguish an absent owned container from an unavailable daemon.
      docker --context default ps >/dev/null
      exit 0
    fi
    test "$owner" = "$state"
    docker --context default rm -f "$container"
    ;;
  *) echo 'usage: critical-media-tools.sh start|stop' >&2; exit 64 ;;
esac
