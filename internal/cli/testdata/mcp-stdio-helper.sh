#!/bin/sh
# MCP stdio fixture for the contained-session interop test: answers
# initialize, ping, tools/list, and tools/call with canned JSON-RPC over
# one line per message. Client notifications get no reply. Only shell
# builtins and /usr/bin/sed are used: the confined child runs under the
# seccomp allowlist, and Landlock roots exclude everything outside the
# runtime prefixes plus the working directory.
extract_id() {
  printf '%s' "$1" | /usr/bin/sed -n 's/.*"id"[ ]*:[ ]*//; s/^\(\("[^"]*"\|[0-9][0-9]*\)\).*/\1/p'
}
while IFS= read -r line; do
  case "$line" in
    *'"method":"ping"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"initialize"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"sh-fixture","version":"1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"ping tool","inputSchema":{"type":"object"}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong mode='"$SERVER_MODE"' home='"${HOME-unset}"'"}]}}\n' "$id"
      ;;
  esac
done
