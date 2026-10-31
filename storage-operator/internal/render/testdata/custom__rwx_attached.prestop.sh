echo 'Shutting down gracefully...'
sleep 5
echo 'Unmounting /shares/media'
timeout 5 umount -f /shares/media 2>/dev/null || umount -l /shares/media 2>/dev/null || echo '  already gone: /shares/media'
echo 'preStop complete'
exit 0