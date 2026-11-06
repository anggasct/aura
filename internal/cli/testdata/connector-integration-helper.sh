#!/bin/sh
field() {
  printf '%s' "$1" | /usr/bin/sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}
while IFS= read -r line; do
  case "$line" in
    *'"type":"describe"'*)
      printf '{"v":"aura.connector/v1","type":"tools","tools":[{"name":"search","description":"search","input_schema":{"type":"object"}},{"name":"send","description":"send","input_schema":{"type":"object"}}]}\n'
      ;;
    *'"type":"invoke"'*)
      call=$(field "$line" call)
      case "$line" in
        *'"mode":"env"'*)
          printf '{"v":"aura.connector/v1","type":"result","call":"%s","output":{"home":"%s"}}\n' "$call" "${HOME-unset}"
          ;;
        *)
          url=$(field "$line" url)
          printf '{"v":"aura.connector/v1","type":"http","call":"%s","method":"GET","url":"%s","headers":{},"body_b64":""}\n' "$call" "$url"
          ;;
      esac
      ;;
    *'"type":"http_result"'*)
      call=$(field "$line" call)
      status=$(printf '%s' "$line" | /usr/bin/sed -n 's/.*"status":\([0-9]*\).*/\1/p')
      printf '{"v":"aura.connector/v1","type":"result","call":"%s","output":{"status":%s}}\n' "$call" "${status:-0}"
      ;;
  esac
done
