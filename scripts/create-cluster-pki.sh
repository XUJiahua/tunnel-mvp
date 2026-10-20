#!/usr/bin/env bash
set -euo pipefail
# Bootstrap a private cluster CA and one server/client certificate per node.
# Keep ca.key offline; distribute only ca.crt and each node's own pair.
if [ "$#" -lt 3 ]; then
  echo 'Usage: create-cluster-pki.sh NEW_DIRECTORY node-a=DNS_OR_IP node-b=DNS_OR_IP [...]' >&2
  exit 2
fi
output=$1
shift
command -v openssl >/dev/null
command -v python3 >/dev/null
# Validate every SAN before creating files or interpreting OpenSSL config text.
python3 - "$@" <<'PY'
import ipaddress,re,sys
seen=set()
for argument in sys.argv[1:]:
    node,sep,host=argument.partition('=')
    if not sep or not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?',node) or node in seen:
        sys.exit('Invalid or duplicate node ID: '+node)
    seen.add(node)
    try:
        ipaddress.ip_address(host)
    except ValueError:
        if len(host)>253 or not all(re.fullmatch(r'[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?',label) for label in host.split('.')):
            sys.exit('Invalid DNS/IP SAN: '+host)
PY
umask 077
mkdir -m 700 -- "$output"
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -x509 -sha256 -days 3650 \
  -subj '/CN=Tunnel private cluster CA' -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' -keyout "$output/ca.key" -out "$output/ca.crt"
for argument in "$@"; do
  node=${argument%%=*}
  host=${argument#*=}
  kind=$(python3 - "$host" <<'PY'
import ipaddress,sys
try:
    ipaddress.ip_address(sys.argv[1]); print('IP')
except ValueError:
    print('DNS')
PY
)
  mkdir -m 700 -- "$output/$node"
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -sha256 \
    -subj "/CN=$node" -keyout "$output/$node/node.key" -out "$output/$node/node.csr"
  cat > "$output/$node/extensions.cnf" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=URI:spiffe://tunnel-mvp/node/$node,$kind:$host
EXT
  openssl x509 -req -in "$output/$node/node.csr" -CA "$output/ca.crt" -CAkey "$output/ca.key" \
    -set_serial "0x$(openssl rand -hex 16)" -days 90 -sha256 -extfile "$output/$node/extensions.cnf" -out "$output/$node/node.crt"
  openssl verify -CAfile "$output/ca.crt" -purpose sslserver "$output/$node/node.crt"
  openssl verify -CAfile "$output/ca.crt" -purpose sslclient "$output/$node/node.crt"
  rm -- "$output/$node/node.csr" "$output/$node/extensions.cnf"
done
printf 'Created private cluster certificates in %s. Keep ca.key offline; leaf certificates expire in 90 days.\n' "$output"
