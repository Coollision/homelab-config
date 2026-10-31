echo 'Shutting down gracefully...'
sleep 5
echo 'Unmounting /shares/docs'
timeout 5 umount -f /shares/docs 2>/dev/null || umount -l /shares/docs 2>/dev/null || echo '  already gone: /shares/docs'
echo 'preStop complete'
exit 0