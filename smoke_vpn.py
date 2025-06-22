import json
import os
import time

def main():
    # Construct the path to result.json relative to the script's location
    # or use an absolute path if preferred.
    # For this example, assuming result.json is in a fixed location.
    file_path = r"C:\Users\husma\OneDrive\Desktop\result.json"
    time.sleep(30)
    try:
        with open(file_path, 'r') as f:
            # Read the file content
            content = f.read()
            # Print the raw JSON string to standard output
            print(content)
    except FileNotFoundError:
        # If the file is not found, print an error message to stderr
        # and return a simple error JSON to stdout
        error_output = {
            "status": "ERROR",
            "messages": [f"Error: File not found at {file_path}"],
            "logs": f"File not found: {file_path}"
        }
        print(json.dumps(error_output))
        # Optionally, exit with a non-zero status code
        # import sys
        # sys.exit(1)
    except Exception as e:
        # For any other errors during file reading or JSON processing
        error_output = {
            "status": "ERROR",
            "messages": [f"An unexpected error occurred: {str(e)}"],
            "logs": f"Error processing file {file_path}: {str(e)}"
        }
        print(json.dumps(error_output))
        # import sys
        # sys.exit(1)

if __name__ == "__main__":
    main()