#!/bin/bash

# 実行回数（引数で指定、デフォルト10回）
COUNT=${1:-30}

# 非同期で curl を実行
for ((i=1; i<=COUNT; i++)); do
  echo "Request #$i"
  curl -i https://ctts.b00-cv001fz.shyme.net/models &
done

# 全 curl プロセスの終了を待つ
wait

echo "All requests completed."
