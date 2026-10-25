Download
# Create a folder
$ mkdir actions-runner && cd actions-runner# Download the latest runner package
$ curl -o actions-runner-osx-x64-2.334.0.tar.gz -L https://github.com/actions/runner/releases/download/v2.334.0/actions-runner-osx-x64-2.334.0.tar.gz# Optional: Validate the hash
$ echo "73a979ff7e9ce8a70244f3a959d896870be486fac92bb08ed90684f961474e0d  actions-runner-osx-x64-2.334.0.tar.gz" | shasum -a 256 -c# Extract the installer
$ tar xzf ./actions-runner-osx-x64-2.334.0.tar.gz
Configure
# Create the runner and start the configuration experience
$ ./config.sh --url https://github.com/danielpaulus/go-ios --token ABBBQZJD7WV2MU6VWFLDE6DJ56XUQ# Last step, run it!
$ ./run.sh
