#!/bin/bash
# データベースの圧縮とサイズ確認

set -e

DB_FILE="data/stock_data.db"
COMPRESSED_FILE="data/stock_data.db.gz"

if [ ! -f "$DB_FILE" ]; then
    echo "❌ Error: $DB_FILE not found"
    exit 1
fi

echo "📦 Compressing database..."
gzip -c "$DB_FILE" > "$COMPRESSED_FILE"

ORIGINAL_SIZE=$(stat -f%z "$DB_FILE" 2>/dev/null || stat -c%s "$DB_FILE")
COMPRESSED_SIZE=$(stat -f%z "$COMPRESSED_FILE" 2>/dev/null || stat -c%s "$COMPRESSED_FILE")

RATIO=$(echo "scale=2; $COMPRESSED_SIZE * 100 / $ORIGINAL_SIZE" | bc)

echo "✅ Compression completed"
echo "   Original:   $(numfmt --to=iec-i --suffix=B $ORIGINAL_SIZE)"
echo "   Compressed: $(numfmt --to=iec-i --suffix=B $COMPRESSED_SIZE)"
echo "   Ratio:      ${RATIO}%"
