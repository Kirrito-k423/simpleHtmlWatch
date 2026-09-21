package watch

// Tab-separated identity and base64 cwd. Validate stat starttime both before and
// after readlink so exited/reused PIDs do not acquire another process's cwd.
const cwdCommand = `printf 'BOOT\t'; cat /proc/sys/kernel/random/boot_id
count=0
for path in /proc/[0-9]*; do
 count=$((count+1)); if [ "$count" -gt 4096 ]; then printf 'LIMIT\t4096\n'; break; fi
 pid=${path##*/}
 a=$(<"$path/stat") 2>/dev/null || continue
 rest=${a##*) }; set -- $rest; started=${20}
 cwd=$(readlink "$path/cwd" 2>/dev/null); rc=$?
 b=$(<"$path/stat") 2>/dev/null; rest=${b##*) }; set -- $rest; ended=${20}
 if [ -z "$started" ] || [ "$started" != "$ended" ]; then
  printf '%s\t%s\tmissing:identity-changed\n' "$pid" "$started"
 elif [ "$rc" != 0 ]; then
  printf '%s\t%s\tmissing:cwd-unavailable\n' "$pid" "$started"
 else
  encoded=$(printf '%s' "$cwd" | base64 | tr -d '\n')
  printf '%s\t%s\t%s\n' "$pid" "$started" "$encoded"
 fi
done`
