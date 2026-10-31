echo 'Shutting down gracefully...'
sleep 5
echo 'Unmounting /shares/old'
timeout 5 umount -f /shares/old 2>/dev/null || umount -l /shares/old 2>/dev/null || echo '  already gone: /shares/old'
echo 'preStop complete'
exit 0