# Parser + validator for clients.conf. Used by reconcile.sh (init container) and by
# scripts/mosquitto-clients-lint.sh (pre-commit/CI), so both always agree on the format.
#
#   awk [-v lint=1] -f clients.awk clients.conf
#
# stdout: one record per line, fields separated by ASCII 0x1f (so topics/passwords may hold spaces):
#   U <name> <password> <flags>   managed user (flags: "readonly" or empty)
#   A <name> <access>  <topic>    access: rw | ro | send
#   X <name>                      retired user: deleted from the broker
# stderr: validation errors (file:line); exit status 1 if any.
# lint=1 additionally requires every password to be a Vault placeholder, so no literal password
# can ever be committed.
function err(msg) { printf "clients.conf:%d: %s\n", NR, msg > "/dev/stderr"; bad = 1 }
BEGIN { US = sprintf("%c", 31) }
/^[ \t]*(#|$)/ { next }
{
  line = $0
  sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line)
  verb = line; sub(/[ \t].*$/, "", verb)
  rest = line; sub(/^[^ \t]+[ \t]*/, "", rest)

  if (verb == "user") {
    n = split(rest, f, /[ \t]+/)
    name = f[1]; pass = f[2]; flag = f[3]
    if (n < 2 || n > 3) { err("usage: user <name> <password> [readonly]"); next }
    if (name !~ /^[A-Za-z0-9_-]+$/) { err("bad user name '" name "'"); next }
    if (name in seen) { err("user '" name "' defined twice"); next }
    if (flag != "" && flag != "readonly") { err("unknown flag '" flag "' (only: readonly)"); next }
    if (lint && pass !~ /^[<]secret:kv\/data\/[^>~]+~[a-z0-9-]+[>]$/) err("password of '" name "' must be a Vault placeholder (secret:kv/data/<path>~<key> in angle brackets), never a literal")
    if (!lint && pass ~ /^[<]secret:/) err("password of '" name "' is an unresolved Vault placeholder")
    seen[name] = 1; cur = name; ro[name] = (flag == "readonly"); acls[name] = 0
    print "U" US name US pass US flag
  } else if (verb == "rw" || verb == "ro" || verb == "send") {
    if (cur == "") { err(verb " before any user"); next }
    if (rest == "") { err("empty topic"); next }
    if (rest ~ /^\$CONTROL/) { err("$CONTROL is reserved for the admin role"); next }
    if (rest == "#" && !ro[cur]) err("bare '#' only for readonly users")
    if (ro[cur] && verb != "ro") err("readonly user '" cur "' may only have ro lines")
    acls[cur]++
    print "A" US cur US verb US rest
  } else if (verb == "retired-user") {
    if (rest !~ /^[A-Za-z0-9_-]+$/) { err("usage: retired-user <name>"); next }
    retired[rest] = NR
    print "X" US rest
  } else {
    err("unknown keyword '" verb "'")
  }
}
END {
  for (u in seen) {
    if (acls[u] == 0) { printf "clients.conf: user '%s' has no access lines\n", u > "/dev/stderr"; bad = 1 }
    if (u in retired) { printf "clients.conf: '%s' is both a user and retired\n", u > "/dev/stderr"; bad = 1 }
  }
  exit bad
}
