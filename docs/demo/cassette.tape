Set Shell zsh
Sleep 1.5s
Type "klimax up"
Sleep 500ms
Enter
Sleep 55s
Type "klimax cluster list"
Sleep 500ms
Enter
Sleep 3s
Type "kubectx"
Enter
Sleep 500ms
Type "klimax cluster create dev"
Enter
Sleep 60s
Type "kubectx"
Sleep 500ms
Enter
Sleep 2s
Type "kubectx dev"
Enter
Sleep 500ms
Type "kubectl get po -A"
Enter
Sleep 1s
Enter
Sleep 2s
Type "klimax cluster e2e-test-nginx"Escape
Enter
Sleep 5.5s
Type "curl 172.30.1.1"
Enter
Sleep 2s
Type "klimax cluster list"
Enter
Type "klimax cluster create staging"
Enter
Sleep 55s
Type "klimax cluster list"
Enter
Sleep 1s
Type "kubectx staging"
Enter
Sleep 1.5s
Type "kubectl get po -A"
Enter
Sleep 3s
Type "klimax cluster e2e-test-nginx"
Enter
Sleep 4.5s
Type "curl 172.30.2.1"
Enter
Sleep 2s
Type "klimax cluster list"
Enter
Sleep 500ms
Type "klimax cluster delete"
Sleep 500ms
Enter
Sleep 500ms
Type " "
Escape
Type "[B "
Enter
Sleep 2.5s
Type "klimax down"
Enter
Sleep 3s

