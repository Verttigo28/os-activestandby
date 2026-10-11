#!/bin/sh
# Creates the key pair that signs the pkg repository.
#   sh build/gen-signing-key.sh
# Prints what to do with the two files. The PRIVATE key never goes into git.
set -e
umask 077
openssl genrsa -out repo.key 4096 2>/dev/null
openssl rsa -in repo.key -pubout -out repo.pub 2>/dev/null
mkdir -p keys
cp repo.pub keys/activestandby.pub
chmod 644 keys/activestandby.pub
echo "private key : repo.key   (keep it secret, do not commit)"
echo "public key  : keys/activestandby.pub   (commit this one)"
echo "fingerprint : $(openssl rsa -pubin -in repo.pub -outform DER 2>/dev/null | openssl dgst -sha256 | sed 's/^.* //')"
echo
echo "Next: GitHub > repository > Settings > Secrets and variables > Actions > New repository secret"
echo "      name  PKG_SIGNING_KEY"
echo "      value the complete content of repo.key, including the BEGIN/END lines"
echo "Then delete repo.key from this machine or keep it in a password manager."
