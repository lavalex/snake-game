package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Define Prometheus Metrics alex
var (
	applesEatenCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "snake_apples_eaten_total",
		Help: "The total number of apples eaten across all submitted games.",
	})

	uniquePlayersGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "snake_leaderboard_unique_players",
		Help: "The number of unique players currently on the leaderboard.",
	})

	playerGamesCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "snake_player_games_total",
			Help: "Number of games completed by specific players.",
		},
		[]string{"player_name"},
	)

	gamesPlayedCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "snake_games_played_total",
		Help: "The total number of completed games (scores submitted) since startup.",
	})

	highScoreGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "snake_high_score",
		Help: "The highest score registered in the system along with the player name.",
	}, []string{"player_name"})

	dbUpGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "snake_db_up",
		Help: "Whether the database connection is currently healthy (1) or not (0).",
	})

	apiRequestsCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "snake_api_requests_total",
			Help: "Total number of HTTP requests handled by endpoint and status.",
		},
		[]string{"exported_endpoint", "method", "status"},
	)
)

type Score struct {
	PlayerName string    `json:"player_name"`
	Score      int       `json:"score"`
	Timestamp  time.Time `json:"timestamp"`
}

// ScoreManager persists scores in PostgreSQL instead of a local JSON file.
type ScoreManager struct {
	db *sql.DB
}

// NewScoreManager opens a connection pool to Postgres, waits for the
// database to become reachable (it may still be starting up alongside this
// pod), and ensures the scores table exists.
func NewScoreManager(dsn string) *ScoreManager {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("[FATAL] Could not initialize database connection: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	const maxAttempts = 30
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[INFO] Waiting for database to become available (attempt %d/%d): %v", attempt, maxAttempts, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("[FATAL] Could not connect to database after %d attempts: %v", maxAttempts, err)
	}
	dbUpGauge.Set(1)

	sm := &ScoreManager{db: db}
	sm.migrate()
	sm.refreshGauges()
	log.Printf("[INFO] Connected to PostgreSQL and ready to serve scores")
	return sm
}

func (sm *ScoreManager) migrate() {
	const schema = `
		CREATE TABLE IF NOT EXISTS scores (
			id          SERIAL PRIMARY KEY,
			player_name VARCHAR(255) NOT NULL,
			score       INTEGER NOT NULL,
			timestamp   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_scores_score ON scores (score DESC);
	`
	if _, err := sm.db.Exec(schema); err != nil {
		log.Fatalf("[FATAL] Failed to run database migration: %v", err)
	}
}

// refreshGauges recomputes the Prometheus gauges from the current DB state.
// Used on startup so metrics reflect data from previous pod restarts.
func (sm *ScoreManager) refreshGauges() {
	var topPlayer string
	var topScore int
	err := sm.db.QueryRow(`SELECT player_name, score FROM scores ORDER BY score DESC LIMIT 1`).Scan(&topPlayer, &topScore)
	highScoreGauge.Reset()
	if err == nil {
		highScoreGauge.WithLabelValues(topPlayer).Set(float64(topScore))
	} else if err != sql.ErrNoRows {
		log.Printf("[ERROR] Failed to read high score for metrics: %v", err)
	}

	var uniquePlayers int
	if err := sm.db.QueryRow(`SELECT COUNT(DISTINCT player_name) FROM scores WHERE player_name <> ''`).Scan(&uniquePlayers); err == nil {
		uniquePlayersGauge.Set(float64(uniquePlayers))
	} else {
		log.Printf("[ERROR] Failed to count unique players for metrics: %v", err)
	}
}

func (sm *ScoreManager) AddScore(playerName string, score int) error {
	if _, err := sm.db.Exec(
		`INSERT INTO scores (player_name, score, timestamp) VALUES ($1, $2, $3)`,
		playerName, score, time.Now(),
	); err != nil {
		return fmt.Errorf("insert score: %w", err)
	}

	// Keep the leaderboard table bounded, mirroring the old "top 100" cap.
	if _, err := sm.db.Exec(`
		DELETE FROM scores WHERE id NOT IN (
			SELECT id FROM scores ORDER BY score DESC LIMIT 100
		)
	`); err != nil {
		log.Printf("[WARN] Failed to prune old scores: %v", err)
	}

	var topPlayer string
	var topScore int
	isNewHighScore := false
	if err := sm.db.QueryRow(`SELECT player_name, score FROM scores ORDER BY score DESC LIMIT 1`).Scan(&topPlayer, &topScore); err == nil {
		if topPlayer == playerName && topScore == score {
			isNewHighScore = true
		}
	}

	sm.refreshGauges()

	log.Printf("[INFO] Score saved successfully. Player: %s | Score: %d", playerName, score)
	if isNewHighScore {
		log.Printf("[HIGH SCORE] %s has reached the top of the leaderboard with a score of %d!", playerName, score)
	}
	return nil
}

func (sm *ScoreManager) GetTopScores(limit int) []Score {
	rows, err := sm.db.Query(
		`SELECT player_name, score, timestamp FROM scores ORDER BY score DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		log.Printf("[ERROR] Failed to query top scores: %v", err)
		return []Score{}
	}
	defer rows.Close()

	scores := []Score{}
	for rows.Next() {
		var s Score
		if err := rows.Scan(&s.PlayerName, &s.Score, &s.Timestamp); err != nil {
			log.Printf("[ERROR] Failed to scan score row: %v", err)
			continue
		}
		scores = append(scores, s)
	}
	return scores
}

// Ping is used for the readiness/liveness probes so Kubernetes knows if the
// app has lost its connection to the database.
func (sm *ScoreManager) Ping() error {
	err := sm.db.Ping()
	if err == nil {
		dbUpGauge.Set(1)
	} else {
		dbUpGauge.Set(0)
	}
	return err
}

var scoreManager *ScoreManager

// buildDSN assembles a PostgreSQL connection string from environment
// variables, matching the credentials injected via the Kubernetes Secret.
func buildDSN() string {
	host := getEnv("DB_HOST", "postgres")
	port := getEnv("DB_PORT", "5432")
	user := getEnv("DB_USER", "snake")
	password := os.Getenv("DB_PASSWORD")
	dbname := getEnv("DB_NAME", "snakegame")
	sslmode := getEnv("DB_SSLMODE", "disable")

	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	scoreManager = NewScoreManager(buildDSN())

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/scores", handleScores)
	http.HandleFunc("/api/start-game", handleStartGame)
	http.HandleFunc("/api/apple-eaten", handleAppleEaten) // NEW HTTP ENDPOINT
	http.HandleFunc("/api/submit-score", handleSubmitScore)
	http.HandleFunc("/healthz", handleHealthz)

	http.Handle("/metrics", promhttp.Handler())

	log.Printf("[INFO] Starting server on port %s", port)
	log.Printf("[INFO] Metrics available at http://localhost:%s/metrics", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	apiRequestsCounter.WithLabelValues("/", r.Method, "200").Inc()
	tmpl := template.Must(template.New("index").Parse(indexHTML))
	tmpl.Execute(w, nil)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := scoreManager.Ping(); err != nil {
		log.Printf("[ERROR] Health check failed, database unreachable: %v", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		apiRequestsCounter.WithLabelValues("/healthz", r.Method, strconv.Itoa(http.StatusServiceUnavailable)).Inc()
		return
	}
	w.WriteHeader(http.StatusOK)
	apiRequestsCounter.WithLabelValues("/healthz", r.Method, "200").Inc()
}

func handleScores(w http.ResponseWriter, r *http.Request) {
	scores := scoreManager.GetTopScores(10)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(scores)

	apiRequestsCounter.WithLabelValues("/api/scores", r.Method, "200").Inc()
}

func handleStartGame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		GameID string `json:"game_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		log.Printf("[INFO] New game session initiated. Remote IP: %s | Game ID: %s", r.RemoteAddr, req.GameID)
	} else {
		log.Printf("[INFO] New game session initiated. Remote IP: %s", r.RemoteAddr)
	}

	w.WriteHeader(http.StatusOK)
	apiRequestsCounter.WithLabelValues("/api/start-game", r.Method, "200").Inc()
}

// NEW FUNCTION: Catches the score update and prints to stdout logs
func handleAppleEaten(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		GameID string `json:"game_id"`
		Score  int    `json:"score"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		log.Printf("[INFO] Apple eaten! Current Score: %d | Game ID: %s | Remote IP: %s", req.Score, req.GameID, r.RemoteAddr)
	}

	w.WriteHeader(http.StatusOK)
	apiRequestsCounter.WithLabelValues("/api/apple-eaten", r.Method, "200").Inc()
}

func handleSubmitScore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("[WARN] Invalid method %s on /api/submit-score from %s", r.Method, r.RemoteAddr)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		apiRequestsCounter.WithLabelValues("/api/submit-score", r.Method, strconv.Itoa(http.StatusMethodNotAllowed)).Inc()
		return
	}

	var req struct {
		GameID     string `json:"game_id"`
		PlayerName string `json:"player_name"`
		Score      int    `json:"score"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[WARN] Failed to decode score payload from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		apiRequestsCounter.WithLabelValues("/api/submit-score", r.Method, strconv.Itoa(http.StatusBadRequest)).Inc()
		return
	}

	if req.PlayerName == "" {
		req.PlayerName = "Anonymous"
	}

	log.Printf("[INFO] Game finished. Player: %s | Score: %d | Game ID: %s", req.PlayerName, req.Score, req.GameID)

	if err := scoreManager.AddScore(req.PlayerName, req.Score); err != nil {
		log.Printf("[ERROR] Failed to save game score for %s: %v", req.PlayerName, err)
		http.Error(w, "Error saving score", http.StatusInternalServerError)
		apiRequestsCounter.WithLabelValues("/api/submit-score", r.Method, strconv.Itoa(http.StatusInternalServerError)).Inc()
		return
	}

	gamesPlayedCounter.Inc()
	applesEaten := req.Score / 10
	applesEatenCounter.Add(float64(applesEaten))
	playerGamesCounter.WithLabelValues(req.PlayerName).Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})

	apiRequestsCounter.WithLabelValues("/api/submit-score", r.Method, "200").Inc()
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Snake Game</title>
    
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            min-height: 100vh;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
        }
        .container {
            background: white;
            padding: 30px;
            border-radius: 20px;
            box-shadow: 0 20px 60px rgba(0,0,0,0.3);
            text-align: center;
        }
        h1 { color: #333; margin-bottom: 20px; }
        .gameCanvas {
            border: 3px solid #333;
            border-radius: 10px;
            display: block;
            margin: 20px auto;
            background: #f0f0f0;
        }
        .score-display {
            font-size: 24px;
            font-weight: bold;
            color: #667eea;
            margin: 15px 0;
        }
        .controls {
            margin: 15px 0;
            color: #666;
            font-size: 14px;
        }
        .leaderboard {
            margin-top: 30px;
            text-align: left;
            max-width: 400px;
        }
        .leaderboard h2 {
            color: #333;
            margin-bottom: 15px;
            text-align: center;
        }
        .score-entry {
            display: flex;
            justify-content: space-between;
            padding: 10px;
            margin: 5px 0;
            background: #f8f9fa;
            border-radius: 5px;
        }
        .score-entry:nth-child(1) { background: #ffd700; }
        .score-entry:nth-child(2) { background: #c0c0c0; }
        .score-entry:nth-child(3) { background: #cd7f32; }
        .modal {
            display: none;
            position: fixed;
            top: 50%;
            left: 50%;
            transform: translate(-50%, -50%);
            background: white;
            padding: 30px;
            border-radius: 15px;
            box-shadow: 0 10px 40px rgba(0,0,0,0.5);
            z-index: 1000;
        }
        .modal input {
            padding: 10px;
            margin: 10px 0;
            border: 2px solid #ddd;
            border-radius: 5px;
            font-size: 16px;
            width: 100%;
        }
        .modal button {
            background: #667eea;
            color: white;
            border: none;
            padding: 12px 30px;
            border-radius: 5px;
            font-size: 16px;
            cursor: pointer;
            margin: 5px;
        }
        .modal button:hover { background: #5568d3; }
        .overlay {
            display: none;
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: rgba(0,0,0,0.7);
            z-index: 999;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>Snake Game</h1>
        by Tangram Soft
        <div class="score-display">Score: <span id="score">0</span></div>
        <canvas id="gameCanvas" class="gameCanvas" width="400" height="400"></canvas>
        <div class="controls">
            Use arrow keys or WASD to move<br>
            Press SPACE to start/restart
        </div>
        <div id="leaderboard" class="leaderboard">
            <h2>Top Scores</h2>
            <div id="scoreList"></div>
        </div>
    </div>

    <div class="overlay" id="overlay"></div>
    <div id="gameOverModal" class="modal">
        <h2>Game Over!</h2>
        <p>Your score: <span id="finalScore">0</span></p>
        <input type="text" id="playerName" placeholder="Enter your name" maxlength="20">
        <div>
            <button onclick="submitScore()">Submit Score</button>
            <button onclick="closeModal()">Cancel</button>
        </div>
    </div>

    <script>
        const canvas = document.getElementById('gameCanvas');
        const ctx = canvas.getContext('2d');
        const gridSize = 20;
        const tileCount = canvas.width / gridSize;

        let snake = [{x: 10, y: 10}];
        let dx = 0, dy = 0;
        let food = {x: 15, y: 15};
        let score = 0;
        let gameRunning = false;
        let gameSpeed = 100;
        let currentGameId = '';

        function drawGame() {
            if (!gameRunning) return;

            moveSnake();
            if (checkCollision()) {
                gameOver();
                return;
            }

            ctx.fillStyle = '#f0f0f0';
            ctx.fillRect(0, 0, canvas.width, canvas.height);

            ctx.fillStyle = '#e74c3c';
            ctx.beginPath();
            ctx.arc(food.x * gridSize + gridSize/2, food.y * gridSize + gridSize/2, gridSize/2 - 2, 0, Math.PI * 2);
            ctx.fill();

            snake.forEach((segment, index) => {
                ctx.fillStyle = index === 0 ? '#27ae60' : '#2ecc71';
                ctx.fillRect(segment.x * gridSize + 1, segment.y * gridSize + 1, gridSize - 2, gridSize - 2);
            });

            setTimeout(drawGame, gameSpeed);
        }

        function moveSnake() {
            if (dx === 0 && dy === 0) return;

            const head = {x: snake[0].x + dx, y: snake[0].y + dy};
            snake.unshift(head);

            if (head.x !== food.x || head.y !== food.y) {
                snake.pop();
            } else {
                score += 10;
                document.getElementById('score').textContent = score;
                
                // NEW: Send a fire-and-forget POST request to the server logs
                fetch('/api/apple-eaten', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({game_id: currentGameId, score: score})
                }).catch(err => console.error('Failed to log apple consumption', err));
                
                placeFood();
                gameSpeed = Math.max(50, 100 - Math.floor(score / 50) * 5);
            }
        }

        function checkCollision() {
            const head = snake[0];
            if (head.x < 0 || head.x >= tileCount || head.y < 0 || head.y >= tileCount) return true;
            for (let i = 1; i < snake.length; i++) {
                if (head.x === snake[i].x && head.y === snake[i].y) return true;
            }
            return false;
        }

        function placeFood() {
            food = {
                x: Math.floor(Math.random() * tileCount),
                y: Math.floor(Math.random() * tileCount)
            };
        }

        async function startGame() {
            snake = [{x: 10, y: 10}];
            dx = 1;
            dy = 0;
            score = 0;
            gameSpeed = 100;
            document.getElementById('score').textContent = score;
            placeFood();
            
            currentGameId = 'game_' + Math.random().toString(36).substr(2, 9) + '_' + Date.now();
            
            fetch('/api/start-game', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({game_id: currentGameId})
            }).catch(err => console.error('Failed to log game start metrics', err));

            gameRunning = true;
            drawGame();
        }

        function gameOver() {
            gameRunning = false;
            document.getElementById('finalScore').textContent = score;
            document.getElementById('overlay').style.display = 'block';
            document.getElementById('gameOverModal').style.display = 'block';
        }

        function closeModal() {
            document.getElementById('overlay').style.display = 'none';
            document.getElementById('gameOverModal').style.display = 'none';
        }

        async function submitScore() {
            const playerName = document.getElementById('playerName').value.trim() || 'Anonymous';
            try {
                await fetch('/api/submit-score', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({
                        game_id: currentGameId,
                        player_name: playerName, 
                        score: score
                    })
                });
                await loadScores();
                closeModal();
            } catch (e) {
                console.error('Error when submitting score:', e);
            }
        }

        async function loadScores() {
            try {
                const response = await fetch('/api/scores');
                const scores = await response.json();
                const scoreList = document.getElementById('scoreList');
                scoreList.innerHTML = scores.map((s, i) => 
                    '<div class="score-entry"><span>' + (i + 1) + '. ' + s.player_name + '</span><span>' + s.score + '</span></div>'
                ).join('');
            } catch (e) {
                console.error('Error loading scores:', e);
            }
        }

        document.addEventListener('keydown', (e) => {
            if (e.code === 'Space') {
                e.preventDefault();
                if (!gameRunning) startGame();
                return;
            }

            if (!gameRunning) return;

            const key = e.key.toLowerCase();
            if ((key === 'arrowup' || key === 'w') && dy === 0) { dx = 0; dy = -1; }
            else if ((key === 'arrowdown' || key === 's') && dy === 0) { dx = 0; dy = 1; }
            else if ((key === 'arrowleft' || key === 'a') && dx === 0) { dx = -1; dy = 0; }
            else if ((key === 'arrowright' || key === 'd') && dx === 0) { dx = 1; dy = 0; }
        });

        loadScores();
    </script>
</body>
</html>`
