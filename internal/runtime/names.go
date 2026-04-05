package runtime

import (
	"fmt"
	"math/rand/v2"
)

// adjectives used for random container name generation.
var adjectives = []string{
	"admiring", "adoring", "affectionate", "agitated", "amazing",
	"angry", "awesome", "beautiful", "blissful", "bold",
	"boring", "brave", "busy", "charming", "clever",
	"compassionate", "competent", "condescending", "confident", "cool",
	"cranky", "crazy", "dazzling", "determined", "distracted",
	"dreamy", "eager", "ecstatic", "elastic", "elated",
	"elegant", "eloquent", "epic", "exciting", "fervent",
	"festive", "flamboyant", "focused", "friendly", "frosty",
	"funny", "gallant", "gifted", "goofy", "gracious",
	"great", "happy", "hardcore", "heuristic", "hopeful",
	"hungry", "infallible", "inspiring", "intelligent", "interesting",
	"jolly", "jovial", "keen", "kind", "laughing",
	"loving", "lucid", "magical", "modest", "musing",
	"mystifying", "naughty", "nervous", "nice", "nifty",
	"nostalgic", "objective", "optimistic", "peaceful", "pedantic",
	"pensive", "practical", "priceless", "quirky", "quizzical",
	"recursing", "relaxed", "reverent", "romantic", "sad",
	"serene", "sharp", "silly", "sleepy", "stoic",
	"strange", "stupefied", "suspicious", "sweet", "tender",
	"thirsty", "trusting", "unruffled", "upbeat", "vibrant",
	"vigilant", "vigorous", "wizardly", "wonderful", "xenodochial",
	"youthful", "zealous", "zen",
}

// names of notable scientists, hackers, and engineers (matching Docker's convention).
var names = []string{
	"albattani", "allen", "almeida", "archimedes", "ardinghelli",
	"babbage", "banach", "bardeen", "bartik", "bassi",
	"bell", "bhabha", "bhaskara", "blackwell", "bohr",
	"booth", "borg", "bose", "boyd", "brahmagupta",
	"brattain", "brown", "carson", "cerf", "chandrasekhar",
	"chatterjee", "colden", "cori", "curie", "darwin",
	"davinci", "dijkstra", "dubinsky", "easley", "einstein",
	"elgamal", "elion", "engelbart", "euclid", "euler",
	"fermat", "fermi", "feynman", "franklin", "gagarin",
	"galileo", "gates", "goldberg", "goldstine", "golick",
	"goodall", "hamilton", "hawking", "heisenberg", "hermann",
	"heyrovsky", "hodgkin", "hoover", "hopper", "hugle",
	"hypatia", "jackson", "jang", "jennings", "jepsen",
	"johnson", "joliot", "jones", "kalam", "kapitsa",
	"kare", "keller", "kepler", "khayyam", "khorana",
	"knuth", "kowalevski", "lalande", "lamarr", "lamport",
	"leakey", "leavitt", "lichterman", "liskov", "lovelace",
	"lumiere", "mahavira", "margulis", "matsumoto", "maxwell",
	"mayer", "mccarthy", "mcclintock", "mclaren", "mclean",
	"mcnulty", "mendel", "mendeleev", "meitner", "mirzakhani",
	"montalcini", "moore", "morse", "moser", "napier",
	"nash", "neumann", "newton", "nightingale", "nobel",
	"noether", "northcutt", "noyce", "panini", "pare",
	"pascal", "pasteur", "payne", "perlman", "pike",
	"poincare", "poitras", "ptolemy", "raman", "ramanujan",
	"ride", "ritchie", "robinson", "roentgen", "rosalind",
	"rubin", "saha", "sammet", "sanderson", "satoshi",
	"shamir", "shannon", "shaw", "shirley", "shockley",
	"sinoussi", "snyder", "solomon", "spence", "stonebraker",
	"sutherland", "swanson", "swartz", "swirles", "taussig",
	"tereshkova", "tesla", "tharp", "thompson", "torvalds",
	"turing", "varahamihira", "villani", "visvesvaraya", "volhard",
	"wescoff", "wilbur", "wiles", "williams", "wilson",
	"wing", "wozniak", "wright", "wu", "yalow",
	"yonath", "zhukovsky",
}

// generateName produces a random Docker-style container name like "jolly_curie42".
func generateName() string {
	adj := adjectives[rand.IntN(len(adjectives))]   //nolint:gosec // Container names don't need cryptographic randomness.
	name := names[rand.IntN(len(names))]             //nolint:gosec // Container names don't need cryptographic randomness.
	num := rand.IntN(100)                            //nolint:gosec // Container names don't need cryptographic randomness.

	return fmt.Sprintf("%s_%s%d", adj, name, num)
}
