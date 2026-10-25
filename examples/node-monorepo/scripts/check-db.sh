#!/bin/sh
# check-db.sh SLOT: succeed when the slot's test database container name is set.
[ -n "${TEST_DB_CONTAINER:-}" ] || { echo "no test database for slot $1" >&2; exit 75; }
