#!/bin/sh
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
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"ping tool","inputSchema":{"type":"object"}},{"name":"hang","description":"never answers","inputSchema":{"type":"object"}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*'"name":"hang"'*|*'"name":"hang"'*'"method":"tools/call"'*)
      while IFS= read -r _; do :; done
      ;;
    *'"method":"tools/call"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong"}]}}\n' "$id"
      ;;
    *'"method"'*)
      id=$(extract_id "$line")
      [ -n "$id" ] && printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unknown"}}\n' "$id"
      ;;
  esac
done
