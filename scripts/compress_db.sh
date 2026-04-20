#!/bin/bash
# 3DB構成の圧縮（xbrl.db, stock_price.db, rs.db）
# レガシー stock_data.db が残っていれば互換のため圧縮する

set -e

DB_FILES=("data/xbrl.db" "data/stock_price.db" "data/rs.db" "data/stock_data.db")
COMPRESSED_ANY=0

for DB_FILE in "${DB_FILES[@]}"; do
    if [ ! -f "$DB_FILE" ]; then
        continue
    fi

    COMPRESSED_FILE="${DB_FILE}.gz"
    echo "📦 Compressing ${DB_FILE}..."
    gzip -c "$DB_FILE" > "$COMPRESSED_FILE"

    ORIGINAL_SIZE=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE")
    COMPRESSED_SIZE=$(stat -f%z "$COMPRESSED_FILE" 2>/dev/null || stat -c%s "$COMPRESSED_FILE")
    RATIO=$(echo "scale=2; $COMPRESSED_SIZE * 100 / $ORIGINAL_SIZE" | bc)

    echo "   Original:   $(numfmt --to=iec-i --suffix=B $ORIGINAL_SIZE)"
    echo "   Compressed: $(numfmt --to=iec-i --suffix=B $COMPRESSED_SIZE)"
    echo "   Ratio:      ${RATIO}%"
    COMPRESSED_ANY=1
done

if [ "$COMPRESSED_ANY" = "0" ]; then
    echo "❌ Error: No DB files found in data/"
    exit 1
fi

echo "✅ Compression completed"
