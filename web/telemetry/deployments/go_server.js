const { spawn } = require('child_process');
const http = require('http');
const fs = require('fs');
const pidFile = './go_server.pid';
const binaryPath = './telemetry_server';

// Launch main.go and get the port
const startMainServer = () => {
    const mainProcess = spawn(binaryPath);
    let mainProcessPid = null;

    mainProcess.stdout.on('data', (data) => {
        const output = data.toString().trim();
        try {
            const match = output.match(/\{"port":[\s]*(\d+)/);
            if (match) {
                const targetPort = parseInt(match[1]);
                console.log(`Main server running on port ${targetPort}`);
                startProxy(targetPort);
            }
        } catch (err) {
            console.error('Error parsing port:', err);
        }
        console.log(data);
    });

    mainProcess.stderr.on('data', (data) => {
        console.error(data.toString());
    });

    mainProcess.on('close', (code) => {
        console.log(`Main server process exited with code ${code}`);
        process.exit(code);
        cleanupPidFile();
    });

    mainProcess.on('error', (err) => {
        console.error("Main process error:", err);
        cleanupPidFile();
    });


    mainProcess.on('exit', (code) => {
        cleanupPidFile();
    });

    mainProcessPid = mainProcess.pid;
    writePidFile(process.pid);
    setInterval(checkPid, 5000);


    // Add event listener to kill server process on exit
    process.on('exit', () => {
        mainProcess.kill();
        cleanupPidFile();
    });
};

const startProxy = (targetPort) => {
    const proxyServer = http.createServer((req, res) => {
        const options = {
            hostname: '127.0.0.1',
            port: targetPort,
            path: req.url,
            method: req.method,
            headers: req.headers
        };

        const proxyReq = http.request(options, (proxyRes) => {
            res.writeHead(proxyRes.statusCode, proxyRes.headers);
            proxyRes.pipe(res);
        });

        req.pipe(proxyReq);
    });

    proxyServer.listen(0, () => {
        console.log('Proxy server listening on port 0');
    });

    // Add event listener to kill proxy server process on exit
    process.on('exit', () => {
        proxyServer.close();
    });
};

const writePidFile = (pid) => {
    fs.writeFileSync(pidFile, ""+pid);
    console.error(`PID file created with PID: ${pid}`);
};

const readPidFile = () => {
    try {
        const data = fs.readFileSync(pidFile, 'utf8');
        const pid = parseInt(data.trim());
        console.error(`PID file read with PID: ${pid}`);
        return pid;
    } catch (err) {
        return null;
    }
};

const checkPid = () => {
    const pidFromFile = readPidFile();
    if (pidFromFile !== null && pidFromFile !== process.pid) {
        console.error(`PID mismatch detected. Expected PID: ${pidFromFile}, Current PID: ${process.pid}`);
        console.error('PID mismatch detected. Shutting down.');
        process.exit(1);
    }
};

const cleanupPidFile = () => {
    try {
        fs.unlinkSync(pidFile);
    } catch (err) {
        console.error("Error cleaning up PID file:", err);
    }
};

startMainServer();