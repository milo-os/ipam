#!/bin/bash
set -euo pipefail

# PostgreSQL Migration Runner
# Applies versioned SQL migrations to a PostgreSQL database.
# Tracks applied migrations in the schema_migrations table.

# Configuration from environment variables
PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-quota}"
PGPASSWORD="${PGPASSWORD:-}"
PGDATABASE="${PGDATABASE:-quota}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-/migrations}"

export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Wait for PostgreSQL to be ready
wait_for_postgres() {
    log_info "Waiting for PostgreSQL to be ready at ${PGHOST}:${PGPORT}..."

    local max_attempts=30
    local attempt=1

    while [ $attempt -le $max_attempts ]; do
        if pg_isready -h "${PGHOST}" -p "${PGPORT}" -U "${PGUSER}" &>/dev/null; then
            log_success "PostgreSQL is ready!"
            return 0
        fi

        log_info "Attempt $attempt/$max_attempts: PostgreSQL not ready yet, waiting..."
        sleep 2
        attempt=$((attempt + 1))
    done

    log_error "PostgreSQL did not become ready within the timeout period"
    return 1
}

# Run a SQL command
psql_cmd() {
    local query="$1"
    psql -t -A -c "${query}" 2>/dev/null
}

# Run a SQL file
psql_file() {
    local file="$1"
    psql -v ON_ERROR_STOP=1 -f "${file}"
}

# Calculate checksum of a file
calculate_checksum() {
    local file="$1"
    sha256sum "${file}" | awk '{print $1}'
}

# Check if a migration has already been applied
is_migration_applied() {
    local version="$1"

    local result
    result=$(psql_cmd "SELECT count(*) FROM schema_migrations WHERE version = ${version}" 2>/dev/null || echo "0")

    [ "${result}" -gt 0 ]
}

# Record a migration as applied
record_migration() {
    local version="$1"
    local name="$2"
    local checksum="$3"

    psql_cmd "INSERT INTO schema_migrations (version, name, checksum) VALUES (${version}, '${name}', '${checksum}')"
}

# Apply a single migration file
apply_migration() {
    local migration_file="$1"
    local filename
    filename=$(basename "${migration_file}")

    # Extract version and name from filename (e.g., 001_initial_schema.sql)
    if [[ ! "${filename}" =~ ^([0-9]{3})_(.+)\.sql$ ]]; then
        log_warning "Skipping ${filename}: doesn't match naming convention {version}_{name}.sql"
        return 0
    fi

    local version="${BASH_REMATCH[1]}"
    local name="${BASH_REMATCH[2]}"
    local version_num=$((10#${version}))  # Convert to decimal, removing leading zeros
    local checksum
    checksum=$(calculate_checksum "${migration_file}")

    # Check if already applied
    if is_migration_applied "${version_num}"; then
        log_info "Migration ${version}_${name} already applied, skipping"
        return 0
    fi

    log_info "Applying migration ${version}_${name}..."

    if psql_file "${migration_file}"; then
        record_migration "${version_num}" "${name}" "${checksum}"
        log_success "Migration ${version}_${name} applied successfully"
        return 0
    else
        log_error "Failed to apply migration ${version}_${name}"
        return 1
    fi
}

# Apply all pending migrations
apply_migrations() {
    log_info "Looking for migration files in ${MIGRATIONS_DIR}..."

    if [ ! -d "${MIGRATIONS_DIR}" ]; then
        log_error "Migrations directory ${MIGRATIONS_DIR} not found"
        return 1
    fi

    # Find all .sql files and sort them by version number
    local migration_files
    migration_files=$(find "${MIGRATIONS_DIR}" -maxdepth 1 -name "*.sql" | sort)

    if [ -z "${migration_files}" ]; then
        log_warning "No migration files found in ${MIGRATIONS_DIR}"
        return 0
    fi

    local migrations_count=0
    local applied_count=0

    while IFS= read -r migration_file; do
        migrations_count=$((migrations_count + 1))
        if apply_migration "${migration_file}"; then
            applied_count=$((applied_count + 1))
        else
            log_error "Migration failed, stopping"
            return 1
        fi
    done <<< "${migration_files}"

    log_success "Migrations complete: ${applied_count} processed out of ${migrations_count} total"
}

# Show current migration status
show_migration_status() {
    log_info "Current migration status:"
    psql_cmd "SELECT version, name, applied_at, substring(checksum, 1, 12) as checksum_short FROM schema_migrations ORDER BY version" || log_warning "Could not fetch migration status"
}

# Main execution
main() {
    log_info "PostgreSQL Migration Runner Starting..."
    log_info "Target: ${PGHOST}:${PGPORT}"
    log_info "Database: ${PGDATABASE}"
    log_info "Migrations Directory: ${MIGRATIONS_DIR}"
    echo ""

    # Wait for PostgreSQL to be ready
    if ! wait_for_postgres; then
        log_error "Failed to connect to PostgreSQL"
        exit 1
    fi

    echo ""

    # Apply all pending migrations
    if ! apply_migrations; then
        log_error "Migration process failed"
        exit 1
    fi

    echo ""

    # Show status
    show_migration_status

    echo ""
    log_success "All migrations completed successfully!"
}

# Run main function
main "$@"
