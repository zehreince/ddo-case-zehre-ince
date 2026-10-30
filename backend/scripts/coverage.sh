#!/bin/sh
# internal/ altındaki paketlerin test kapsamını ölçer ve eşiğin altındaysa
# başarısız olur. -coverpkg sayesinde bir paketin başka paketin testleriyle
# çalışan kodu da sayılır. cmd/ ince giriş noktalarıdır, ölçüme girmez.
#
# Kullanım: scripts/coverage.sh [eşik]   (varsayılan 70)
set -eu
cd "$(dirname "$0")/.."

THRESHOLD="${1:-70}"
go test ${GO_TEST_FLAGS:-} -coverpkg=./internal/... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out > coverage.txt

TOTAL=$(awk '/^total:/ { sub("%", "", $3); print $3 }' coverage.txt)
echo "internal/ kapsama: ${TOTAL}% (eşik ${THRESHOLD}%)"

awk -v t="$TOTAL" -v min="$THRESHOLD" 'BEGIN { exit (t + 0 >= min + 0) ? 0 : 1 }' || {
  echo "kapsama eşiğin altında" >&2
  exit 1
}
