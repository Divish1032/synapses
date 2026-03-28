package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Solidity test helpers ──────────────────────────────────────────────────

const basicSolidityContract = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

contract SimpleStorage {
    uint public storedValue;

    function setValue(uint value) public {
        storedValue = value;
    }

    function getValue() public view returns (uint) {
        return storedValue;
    }
}
`

const solidityWithImports = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "./helpers.sol";
import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";

contract MyToken is ERC20, Ownable {
    constructor() ERC20("MyToken", "MT") {}
}
`

const solidityWithInheritance = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

abstract contract Base {
    function getValue() public virtual returns (uint);
}

contract Derived is Base {
    uint internal value;

    function getValue() public override returns (uint) {
        return value;
    }

    function setValue(uint _value) public {
        value = _value;
    }
}
`

const solidityWithFunctions = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

contract Calculator {
    uint public result;

    function add(uint a, uint b) public {
        result = a + b;
    }

    function subtract(uint a, uint b) public {
        result = a - b;
    }

    function multiply(uint a, uint b) public {
        result = a * b;
    }

    function divide(uint a, uint b) public {
        require(b != 0, "Division by zero");
        result = a / b;
    }
}
`

const solidityMinimal = `pragma solidity ^0.8.0;

contract Empty {
}
`

const solidityWithModifiers = `pragma solidity ^0.8.0;

contract Access {
    address public owner;

    modifier onlyOwner() {
        require(msg.sender == owner, "Not owner");
        _;
    }

    function restricted() public onlyOwner {
        // Only owner can call
    }

    function publicFunction() public {
        // Anyone can call
    }
}
`

const solidityWithEvents = `pragma solidity ^0.8.0;

contract EventExample {
    event Transfer(address indexed from, address indexed to, uint value);
    event Approval(address indexed owner, address indexed spender, uint value);

    function transfer(address to, uint value) public {
        emit Transfer(msg.sender, to, value);
    }
}
`

const solidityWithStructs = `pragma solidity ^0.8.0;

contract StructExample {
    struct Person {
        string name;
        uint age;
        address account;
    }

    struct Company {
        string name;
        address owner;
        uint employees;
    }

    Person public founder;
    Company public organization;
}
`

func parseSolidity(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSolidityParser()
	if err := p.Parse(g, "/tmp/test.sol", []byte(src)); err != nil {
		t.Fatalf("SolidityParser.Parse() error: %v", err)
	}
	return g
}

func parseSolidityWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSolidityParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("SolidityParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestSolidityParser_Extensions(t *testing.T) {
	exts := parser.NewSolidityParser().Extensions()
	if len(exts) < 1 {
		t.Errorf("Extensions() = %v, want at least 1 extension", exts)
	}
	// Should support .sol
	hasSol := false
	for _, ext := range exts {
		if ext == ".sol" {
			hasSol = true
			break
		}
	}
	if !hasSol {
		t.Errorf("Extensions() = %v, want to include .sol", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestSolidityParser_FileNode(t *testing.T) {
	g := parseSolidity(t, basicSolidityContract)
	nodes := g.FindByName("test.sol")
	if len(nodes) == 0 {
		t.Fatal("file node test.sol not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Basic contract ─────────────────────────────────────────────────────────

func TestSolidityParser_BasicContract(t *testing.T) {
	g := parseSolidityWithFilename(t, "Storage.sol", basicSolidityContract)

	// Check for file node
	fileNodes := g.FindByName("Storage.sol")
	if len(fileNodes) == 0 {
		t.Fatal("file node Storage.sol not found")
	}

	// Check for contract node
	contractNodes := g.FindByName("SimpleStorage")
	if len(contractNodes) == 0 {
		t.Fatal("expected SimpleStorage contract")
	}
}

// ─── Imports ────────────────────────────────────────────────────────────────

func TestSolidityParser_Imports(t *testing.T) {
	g := parseSolidityWithFilename(t, "Token.sol", solidityWithImports)

	// Check for file node
	fileNodes := g.FindByName("Token.sol")
	if len(fileNodes) == 0 {
		t.Fatal("file node Token.sol not found")
	}

	// Check for contract node
	contractNodes := g.FindByName("MyToken")
	if len(contractNodes) == 0 {
		t.Fatal("expected MyToken contract")
	}
}

// ─── Inheritance ────────────────────────────────────────────────────────────

func TestSolidityParser_Inheritance(t *testing.T) {
	g := parseSolidityWithFilename(t, "Derived.sol", solidityWithInheritance)

	// Check for base contract
	baseNodes := g.FindByName("Base")
	if len(baseNodes) == 0 {
		t.Fatal("expected Base contract")
	}

	// Check for derived contract
	derivedNodes := g.FindByName("Derived")
	if len(derivedNodes) == 0 {
		t.Fatal("expected Derived contract")
	}
}

// ─── Functions ──────────────────────────────────────────────────────────────

func TestSolidityParser_Functions(t *testing.T) {
	g := parseSolidityWithFilename(t, "Calculator.sol", solidityWithFunctions)

	// Check for contract
	contractNodes := g.FindByName("Calculator")
	if len(contractNodes) == 0 {
		t.Fatal("expected Calculator contract")
	}

	// Check for functions
	addNodes := g.FindByName("add")
	if len(addNodes) == 0 {
		t.Fatal("expected add function")
	}

	subtractNodes := g.FindByName("subtract")
	if len(subtractNodes) == 0 {
		t.Fatal("expected subtract function")
	}
}

// ─── Minimal contract ───────────────────────────────────────────────────────

func TestSolidityParser_Minimal(t *testing.T) {
	g := parseSolidity(t, solidityMinimal)

	fileNodes := g.FindByName("test.sol")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}

	contractNodes := g.FindByName("Empty")
	if len(contractNodes) == 0 {
		t.Fatal("expected Empty contract")
	}
}

// ─── Modifiers ──────────────────────────────────────────────────────────────

func TestSolidityParser_Modifiers(t *testing.T) {
	g := parseSolidityWithFilename(t, "Access.sol", solidityWithModifiers)

	// Check for contract
	contractNodes := g.FindByName("Access")
	if len(contractNodes) == 0 {
		t.Fatal("expected Access contract")
	}

	// Check for modifier
	modifierNodes := g.FindByName("onlyOwner")
	if len(modifierNodes) == 0 {
		t.Fatal("expected onlyOwner modifier")
	}
}

// ─── Events ─────────────────────────────────────────────────────────────────

func TestSolidityParser_Events(t *testing.T) {
	g := parseSolidityWithFilename(t, "EventExample.sol", solidityWithEvents)

	// Check for contract
	contractNodes := g.FindByName("EventExample")
	if len(contractNodes) == 0 {
		t.Fatal("expected EventExample contract")
	}

	// Check for events
	transferNodes := g.FindByName("Transfer")
	if len(transferNodes) == 0 {
		t.Fatal("expected Transfer event")
	}

	approvalNodes := g.FindByName("Approval")
	if len(approvalNodes) == 0 {
		t.Fatal("expected Approval event")
	}
}

// ─── Structs ────────────────────────────────────────────────────────────────

func TestSolidityParser_Structs(t *testing.T) {
	g := parseSolidityWithFilename(t, "StructExample.sol", solidityWithStructs)

	// Check for contract
	contractNodes := g.FindByName("StructExample")
	if len(contractNodes) == 0 {
		t.Fatal("expected StructExample contract")
	}

	// Verify file was parsed successfully
	fileNodes := g.FindByName("StructExample.sol")
	if len(fileNodes) == 0 {
		t.Fatal("expected file node")
	}
}

// ─── Complex contract ───────────────────────────────────────────────────────

func TestSolidityParser_ComplexContract(t *testing.T) {
	src := `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import "@openzeppelin/contracts/access/Ownable.sol";

contract MyToken is ERC20, Ownable {
    event MintEvent(address indexed to, uint256 amount);
    event BurnEvent(address indexed from, uint256 amount);

    modifier validAmount(uint256 amount) {
        require(amount > 0, "Amount must be positive");
        _;
    }

    constructor() ERC20("MyToken", "MTK") {}

    function mint(address to, uint256 amount) public onlyOwner validAmount(amount) {
        _mint(to, amount);
        emit MintEvent(to, amount);
    }

    function burn(uint256 amount) public validAmount(amount) {
        _burn(msg.sender, amount);
        emit BurnEvent(msg.sender, amount);
    }
}
`
	g := parseSolidityWithFilename(t, "Token.sol", src)

	// Check for contract
	contractNodes := g.FindByName("MyToken")
	if len(contractNodes) == 0 {
		t.Fatal("expected MyToken contract")
	}

	// Check for functions
	mintNodes := g.FindByName("mint")
	if len(mintNodes) == 0 {
		t.Fatal("expected mint function")
	}

	burnNodes := g.FindByName("burn")
	if len(burnNodes) == 0 {
		t.Fatal("expected burn function")
	}

	// Check for events
	mintEventNodes := g.FindByName("MintEvent")
	if len(mintEventNodes) == 0 {
		t.Fatal("expected MintEvent event")
	}
}

// ─── Empty contract ─────────────────────────────────────────────────────────

func TestSolidityParser_Empty(t *testing.T) {
	g := parseSolidity(t, "")

	// Should still create a file node
	fileNodes := g.FindByName("test.sol")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist for empty solidity")
	}
}
