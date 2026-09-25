import subprocess, sys
PENDING = sys.argv[1:]          # 允许仍然缺失的文件（后续任务创建）
out = subprocess.run(['python3','scripts/check_links.py'],
                     capture_output=True, text=True).stdout
bad = [l.strip() for l in out.splitlines() if '指向不存在' in l or '锚点缺失' in l]
unexpected = [l for l in bad if not any(p in l for p in PENDING)]
print('坏链接 %d 处；允许待创建 %d 处；意外 %d 处'
      % (len(bad), len(bad)-len(unexpected), len(unexpected)))
for l in unexpected:
    print('  意外: ' + l)
sys.exit(1 if unexpected else 0)
